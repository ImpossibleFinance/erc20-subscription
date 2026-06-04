package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore persists state to Redis. Schema:
//
//	plan:<id>            STRING (json)
//	plans                SET    (plan IDs)
//	sub:<address>        STRING (json)
//	subs:<status>        SET    (addresses)
//	subs:due             ZSET   (member=address, score=NextAttemptAt unix)
//	session:<id>         STRING (json)
//	sessions:expiring    ZSET   (member=session id, score=ExpiresAt unix; pending only)
//	tx:<txHash>          STRING (session id that owns this transfer hash)
//
// All addresses + tx hashes are stored lowercase; callers must normalize.
type RedisStore struct{ r *redis.Client }

func NewRedis(url string) (*RedisStore, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	return NewRedisFromClient(redis.NewClient(opts)), nil
}

// NewRedisFromClient lets the caller share a single *redis.Client between
// the store, the scheduler lock, and any other Redis-backed primitives.
func NewRedisFromClient(r *redis.Client) *RedisStore { return &RedisStore{r: r} }

// Client returns the underlying Redis client, for callers that need to
// construct adjacent primitives (locker, rate limiter, etc.) against the
// same connection pool.
func (s *RedisStore) Client() *redis.Client { return s.r }

// ---------- plans ----------

func (s *RedisStore) UpsertPlan(ctx context.Context, p *Plan) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	pipe := s.r.TxPipeline()
	pipe.Set(ctx, "plan:"+p.ID, b, 0)
	pipe.SAdd(ctx, "plans", p.ID)
	_, err = pipe.Exec(ctx)
	return err
}

func (s *RedisStore) GetPlan(ctx context.Context, id string) (*Plan, error) {
	raw, err := s.r.Get(ctx, "plan:"+id).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *RedisStore) ListPlans(ctx context.Context) ([]*Plan, error) {
	ids, err := s.r.SMembers(ctx, "plans").Result()
	if err != nil {
		return nil, err
	}
	out := make([]*Plan, 0, len(ids))
	for _, id := range ids {
		p, err := s.GetPlan(ctx, id)
		if err != nil {
			return nil, err
		}
		if p != nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// ---------- subscriptions ----------

func (s *RedisStore) UpsertSubscription(ctx context.Context, sub *Subscription) error {
	sub.User = strings.ToLower(sub.User)
	sub.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(sub)
	if err != nil {
		return err
	}

	pipe := s.r.TxPipeline()
	pipe.Set(ctx, "sub:"+sub.User, b, 0)
	for _, st := range []string{StatusActive, StatusPastDue, StatusCancelled} {
		pipe.SRem(ctx, "subs:"+st, sub.User)
	}
	pipe.SAdd(ctx, "subs:"+sub.Status, sub.User)

	billable := sub.Status == StatusActive || sub.Status == StatusPastDue
	if billable {
		// Pending one-time charges score at -inf so they always sort first
		// in DueBefore — gets processed as soon as the scheduler can.
		score := float64(sub.NextAttemptAt.Unix())
		if sub.PendingChargeAtomic != "" {
			score = 0
		}
		pipe.ZAdd(ctx, "subs:due", redis.Z{Score: score, Member: sub.User})
	} else {
		pipe.ZRem(ctx, "subs:due", sub.User)
	}

	_, err = pipe.Exec(ctx)
	return err
}

func (s *RedisStore) GetSubscription(ctx context.Context, user string) (*Subscription, error) {
	raw, err := s.r.Get(ctx, "sub:"+strings.ToLower(user)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sub Subscription
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

func (s *RedisStore) ListSubscriptions(ctx context.Context, status string) ([]*Subscription, error) {
	var users []string
	var err error
	if status == "" {
		users, err = s.r.SUnion(ctx,
			"subs:"+StatusActive, "subs:"+StatusPastDue, "subs:"+StatusCancelled,
		).Result()
	} else {
		users, err = s.r.SMembers(ctx, "subs:"+status).Result()
	}
	if err != nil {
		return nil, err
	}
	out := make([]*Subscription, 0, len(users))
	for _, u := range users {
		sub, err := s.GetSubscription(ctx, u)
		if err != nil {
			return nil, err
		}
		if sub != nil {
			out = append(out, sub)
		}
	}
	return out, nil
}

// ---------- sessions ----------

func (s *RedisStore) CreateSession(ctx context.Context, sess *CheckoutSession) error {
	b, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	pipe := s.r.TxPipeline()
	pipe.Set(ctx, "session:"+sess.ID, b, 0)
	if sess.Status == SessionStatusPending {
		pipe.ZAdd(ctx, "sessions:expiring", redis.Z{
			Score:  float64(sess.ExpiresAt.Unix()),
			Member: sess.ID,
		})
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *RedisStore) GetSession(ctx context.Context, id string) (*CheckoutSession, error) {
	raw, err := s.r.Get(ctx, "session:"+id).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sess CheckoutSession
	if err := json.Unmarshal(raw, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

// CompleteSession is a CAS via Redis WATCH. It atomically checks the session
// is pending, that no OTHER session owns this transfer-tx hash, then writes
// the completed session + reserves the tx hash + drops the session from the
// expiring index. Concurrent callers retry on the WATCH race.
func (s *RedisStore) CompleteSession(ctx context.Context, id, wallet, transferTx, approvalTx, subscriptionID string, at time.Time) error {
	hash := strings.ToLower(transferTx)
	sessionKey := "session:" + id
	txKey := "tx:" + hash

	for attempt := 0; attempt < 5; attempt++ {
		err := s.r.Watch(ctx, func(tx *redis.Tx) error {
			rawSess, err := tx.Get(ctx, sessionKey).Bytes()
			if err == redis.Nil {
				return ErrSessionNotFound
			}
			if err != nil {
				return err
			}
			var sess CheckoutSession
			if err := json.Unmarshal(rawSess, &sess); err != nil {
				return err
			}
			if sess.Status != SessionStatusPending {
				return ErrSessionNotPending
			}

			owner, err := tx.Get(ctx, txKey).Result()
			if err != nil && err != redis.Nil {
				return err
			}
			if err != redis.Nil && owner != id {
				return ErrTxAlreadyConsumed
			}

			completed := at
			sess.Status = SessionStatusCompleted
			sess.Wallet = strings.ToLower(wallet)
			sess.InitialTransferTx = hash
			sess.ApprovalTxHash = strings.ToLower(approvalTx)
			sess.SubscriptionID = subscriptionID
			sess.CompletedAt = &completed

			updated, err := json.Marshal(&sess)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.Set(ctx, sessionKey, updated, 0)
				p.Set(ctx, txKey, id, 0)
				p.ZRem(ctx, "sessions:expiring", id)
				return nil
			})
			return err
		}, sessionKey, txKey)

		if err == redis.TxFailedErr {
			continue // WATCH race; retry
		}
		return err
	}
	return redis.TxFailedErr
}

func (s *RedisStore) ExpireSessionsBefore(ctx context.Context, t time.Time) (int, error) {
	ids, err := s.r.ZRangeByScore(ctx, "sessions:expiring", &redis.ZRangeBy{
		Min:   "-inf",
		Max:   strconv.FormatInt(t.Unix(), 10),
		Count: 1000,
	}).Result()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		// Read-modify-write under WATCH per id, so we don't race a concurrent
		// CompleteSession.
		err := s.r.Watch(ctx, func(tx *redis.Tx) error {
			rawSess, err := tx.Get(ctx, "session:"+id).Bytes()
			if err == redis.Nil {
				// Vanished — drop from the index and move on.
				_, _ = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
					p.ZRem(ctx, "sessions:expiring", id)
					return nil
				})
				return nil
			}
			if err != nil {
				return err
			}
			var sess CheckoutSession
			if err := json.Unmarshal(rawSess, &sess); err != nil {
				return err
			}
			if sess.Status != SessionStatusPending {
				// Completed or already-expired sessions just drop from the
				// expiring index; nothing else to do.
				_, _ = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
					p.ZRem(ctx, "sessions:expiring", id)
					return nil
				})
				return nil
			}
			sess.Status = SessionStatusExpired
			updated, err := json.Marshal(&sess)
			if err != nil {
				return err
			}
			pipe := tx.TxPipeline()
			pipe.Set(ctx, "session:"+id, updated, 0)
			pipe.ZRem(ctx, "sessions:expiring", id)
			// Release any ghost tx reservation (a pending session may have
			// stashed a hash but never reached Complete — extraordinarily
			// rare; we belt-and-braces it).
			if sess.InitialTransferTx != "" {
				pipe.Del(ctx, "tx:"+sess.InitialTransferTx)
			}
			if _, err := pipe.Exec(ctx); err != nil {
				return err
			}
			n++
			return nil
		}, "session:"+id)

		if err == redis.TxFailedErr {
			// Lost the race to a concurrent CompleteSession; that's fine —
			// the session is now completed and out of the expiring index.
			continue
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (s *RedisStore) DueBefore(ctx context.Context, t time.Time, limit int) ([]*Subscription, error) {
	users, err := s.r.ZRangeByScore(ctx, "subs:due", &redis.ZRangeBy{
		Min:   "-inf",
		Max:   strconv.FormatInt(t.Unix(), 10),
		Count: int64(limit),
	}).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*Subscription, 0, len(users))
	for _, u := range users {
		sub, err := s.GetSubscription(ctx, u)
		if err != nil {
			return nil, err
		}
		if sub != nil {
			out = append(out, sub)
		}
	}
	return out, nil
}
