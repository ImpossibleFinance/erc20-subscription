package store

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisLocker implements scheduler.Locker via SETNX + Lua compare-and-swap.
// See scheduler.Locker for semantics.
//
// Uses Lua for Renew/Release so we never extend or delete a lock that's
// been taken over (`GET == holderID` check is atomic with the EXPIRE/DEL).
type RedisLocker struct{ r *redis.Client }

func NewRedisLocker(r *redis.Client) *RedisLocker { return &RedisLocker{r: r} }

func (l *RedisLocker) Acquire(ctx context.Context, key, holder string, ttl time.Duration) (bool, error) {
	return l.r.SetNX(ctx, key, holder, ttl).Result()
}

var renewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)

func (l *RedisLocker) Renew(ctx context.Context, key, holder string, ttl time.Duration) error {
	res, err := renewScript.Run(ctx, l.r, []string{key}, holder, int64(ttl/time.Millisecond)).Int64()
	if err != nil {
		return err
	}
	if res == 0 {
		return ErrLostLock
	}
	return nil
}

var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func (l *RedisLocker) Release(ctx context.Context, key, holder string) error {
	_, err := releaseScript.Run(ctx, l.r, []string{key}, holder).Int64()
	return err
}

// ErrLostLock is returned by Renew when the lock is no longer ours — another
// instance grabbed it after our TTL expired (e.g. we paused too long).
var ErrLostLock = errors.New("lock lost: another instance holds it")
