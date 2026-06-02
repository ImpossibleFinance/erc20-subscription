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
//	plan:<id>            HASH (json)
//	plans                SET   (plan IDs)
//	sub:<address>        HASH (json)
//	subs:<status>        SET  (addresses)
//	subs:due             ZSET (member=address, score=NextAttemptAt unix)
//
// All addresses are stored lowercase; callers must normalize at the edge.
type RedisStore struct{ r *redis.Client }

func NewRedis(url string) (*RedisStore, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	return &RedisStore{r: redis.NewClient(opts)}, nil
}

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
