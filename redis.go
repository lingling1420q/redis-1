package redis

import (
	"context"
	"net"
	"time"

	"git.bilibili.co/go/net/pool"
	"git.bilibili.co/go/redis/protocol"
)

const (
	FlagNX = 1 << 0
	FlagXX = 1 << 1
	FlagCH = 1 << 2
)

type Options struct {
	Address      string
	PoolSize     int
	MinIdleConns int

	MaxConnAge  time.Duration
	PoolTimeout time.Duration
	IdleTimeout time.Duration

	IdleCheckFrequency time.Duration
}

type Client struct {
	pool pool.Pooler
}

func New(opts Options) Client {
	poolOpts := pool.Options{
		PoolSize:           opts.PoolSize,
		MinIdleConns:       opts.MinIdleConns,
		MaxConnAge:         opts.MaxConnAge,
		PoolTimeout:        opts.PoolTimeout,
		IdleTimeout:        opts.IdleTimeout,
		IdleCheckFrequency: opts.IdleCheckFrequency,
	}

	poolOpts.Dialer = func(ctx context.Context) (pool.Closer, error) {
		d := net.Dialer{}
		conn, err := d.DialContext(ctx, "tcp", opts.Address)
		if err != nil {
			return nil, err
		}

		rw := redisConn{
			c: conn,
			r: protocol.NewReader(conn),
			w: protocol.NewWriter(conn),
		}

		return &rw, nil
	}

	return Client{pool: pool.New(poolOpts)}
}

type redisConn struct {
	c net.Conn
	r *protocol.Reader
	w *protocol.Writer
}

func (rc *redisConn) Close() error {
	return rc.c.Close()
}

type ZSetValue struct {
	Member string
	Score  float64
}

type Item struct {
	// Key is the Item's key (250 bytes maximum).
	Key string

	// Value is the Item's value.
	Value []byte

	ZSetValues map[string]float64

	// Flags 一些 redis 标记位，请参考 Flag 开头的常量定义
	Flags uint32

	// TTL 缓存时间，秒，0 表示不过期
	TTL int32
}

var noDeadline = time.Time{}

func (c *Client) do(ctx context.Context, cmd string, fn func(conn *redisConn) error) error {
	conn, err := c.pool.Get(ctx)
	if err != nil {
		return err
	}

	defer func() {
		if IsBadConn(err, false) {
			c.pool.Remove(conn)
		} else {
			c.pool.Put(conn)
		}

	}()

	rc := conn.C.(*redisConn)

	if t, ok := ctx.Deadline(); ok {
		err = rc.c.SetDeadline(t)
	} else {
		err = rc.c.SetDeadline(noDeadline)
	}

	if err != nil {
		return err
	}

	// 此处赋值给 defer 函数用的，不要去掉
	err = fn(rc)

	return err
}

func (c *Client) Get(ctx context.Context, key string) (item *Item, err error) {
	cmd := "get"
	err = c.do(ctx, cmd, func(conn *redisConn) error {
		args := []interface{}{cmd, key}

		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		var b []byte
		if b, err = conn.r.ReadBytesReply(); err != nil {
			item = nil
			return err
		}

		item = &Item{Value: b}

		return nil
	})
	return
}

func (c *Client) MGet(ctx context.Context, keys []string) (items map[string]*Item, err error) {
	cmd := "mget"
	err = c.do(ctx, cmd, func(conn *redisConn) error {
		args := make([]interface{}, 0, len(keys)+1)

		args = append(args, cmd)
		for _, key := range keys {
			args = append(args, key)
		}

		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		l, err := conn.r.ReadArrayLenReply()
		if err != nil {
			return err
		}

		items := make(map[string]*Item, l)

		for i := 0; i < l; i++ {
			b, err := conn.r.ReadBytesReply()
			if err != nil && err != Nil {
				return err
			}

			key := keys[i]

			items[key] = &Item{Value: b}
		}

		return nil
	})
	return
}

func (c *Client) Set(ctx context.Context, item *Item) error {
	cmd := "set"
	args := make([]interface{}, 0, 6)
	args = append(args, cmd, item.Key, item.Value)

	if item.TTL > 0 {
		args = append(args, "expiration", "EX", item.TTL)
	}

	if item.Flags&FlagNX > 0 {
		args = append(args, "NX")
	} else if item.Flags&FlagXX > 0 {
		args = append(args, "XX")
	}

	return c.do(ctx, cmd, func(conn *redisConn) error {
		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		_, err := conn.r.ReadStatusReply()
		if err != nil {
			return err
		}

		return nil
	})
}

func (c *Client) Del(ctx context.Context, keys ...string) error {
	cmd := "del"
	args := make([]interface{}, 0, 1+len(keys))

	args = append(args, cmd)
	for _, key := range keys {
		args = append(args, key)
	}

	return c.do(ctx, cmd, func(conn *redisConn) error {
		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		_, err := conn.r.ReadIntReply()

		return err
	})
}

func (c *Client) IncrBy(ctx context.Context, key string, by int64) (i int64, err error) {
	cmd := "incrby"
	err = c.do(ctx, cmd, func(conn *redisConn) error {
		args := []interface{}{cmd, key, by}

		if err = conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		i, err = conn.r.ReadIntReply()

		return err
	})

	return
}

func (c *Client) DecrBy(ctx context.Context, key string, by int64) (int64, error) {
	return c.IncrBy(ctx, key, -by)
}

func (c *Client) Expire(ctx context.Context, key string, ttl int32) error {
	cmd := "expire"
	return c.do(ctx, cmd, func(conn *redisConn) error {
		args := []interface{}{cmd, key, ttl}
		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		_, err := conn.r.ReadIntReply()

		return err
	})
}

func (c *Client) TTL(ctx context.Context, key string) (ttl int32, err error) {
	cmd := "ttl"
	err = c.do(ctx, cmd, func(conn *redisConn) error {
		args := []interface{}{cmd, key}
		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		var i int64
		i, err = conn.r.ReadIntReply()
		if err != nil {
			return err
		}

		if i == -2 {
			err = Nil
			return err
		}

		ttl = int32(i)

		return err
	})

	return
}

func (c *Client) ZAdd(ctx context.Context, item *Item) (added int64, err error) {
	cmd := "zadd"
	args := make([]interface{}, 0, 4+len(item.ZSetValues))
	args = append(args, cmd, item.Key)

	if item.Flags&FlagNX > 0 {
		args = append(args, "NX")
	} else if item.Flags&FlagXX > 0 {
		args = append(args, "XX")
	}

	if item.Flags&FlagCH > 0 {
		args = append(args, "CH")
	}

	for member, score := range item.ZSetValues {
		args = append(args, score, member)
	}

	err = c.do(ctx, cmd, func(conn *redisConn) error {
		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		added, err = conn.r.ReadIntReply()
		if err != nil {
			return err
		}

		return nil
	})

	return
}

func (c *Client) ZIncrBy(ctx context.Context, key, member string, by float64) error {
	cmd := "zincrby"
	return c.do(ctx, cmd, func(conn *redisConn) error {
		args := []interface{}{cmd, key, by, member}
		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		_, err := conn.r.ReadIntReply()
		return err
	})
}

func (c *Client) ZRange(ctx context.Context, key string, start, stop int64) (values []*ZSetValue, err error) {
	return c.zrange(ctx, "zrange", key, start, stop)
}

func (c *Client) ZRevRange(ctx context.Context, key string, start, stop int64) (values []*ZSetValue, err error) {
	return c.zrange(ctx, "zrevrange", key, start, stop)
}

func (c *Client) zrange(ctx context.Context, cmd, key string, start, stop int64) (values []*ZSetValue, err error) {
	err = c.do(ctx, cmd, func(conn *redisConn) error {
		args := []interface{}{cmd, key, start, stop, "WITHSCORES"}

		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		l, err := conn.r.ReadArrayLenReply()
		if err != nil {
			return err
		}

		values = make([]*ZSetValue, 0, l)
		for i := 0; i < l/2; i++ {
			b, err := conn.r.ReadBytesReply()
			if err != nil {
				return err
			}
			f, err := conn.r.ReadFloat()
			if err != nil {
				return err
			}

			values = append(values, &ZSetValue{Member: string(b), Score: f})
		}

		return nil
	})

	return
}

func (c *Client) ZRank(ctx context.Context, key, member string) (rank int64, err error) {
	return c.zrank(ctx, "zrank", key, member)
}
func (c *Client) ZRevRank(ctx context.Context, key, member string) (rank int64, err error) {
	return c.zrank(ctx, "zrevrank", key, member)
}

func (c *Client) zrank(ctx context.Context, cmd, key, member string) (rank int64, err error) {
	err = c.do(ctx, cmd, func(conn *redisConn) error {
		args := []interface{}{cmd, key, member}
		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		rank, err = conn.r.ReadIntReply()
		return err
	})

	return
}

func (c *Client) ZScore(ctx context.Context, key, member string) (score float64, err error) {
	cmd := "zscore"
	err = c.do(ctx, cmd, func(conn *redisConn) error {
		args := []interface{}{cmd, key, member}
		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		score, err = conn.r.ReadFloat()
		return err
	})

	return
}

func (c *Client) ZCard(ctx context.Context, key string) (card int64, err error) {
	cmd := "zcard"
	err = c.do(ctx, cmd, func(conn *redisConn) error {
		args := []interface{}{cmd, key}
		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		card, err = conn.r.ReadIntReply()

		return err
	})
	return
}

func (c *Client) ZCount(ctx context.Context, key, min, max string) (i int64, err error) {
	cmd := "zcount"
	err = c.do(ctx, cmd, func(conn *redisConn) error {
		args := []interface{}{cmd, key, min, max}
		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		i, err = conn.r.ReadIntReply()

		return err
	})
	return
}

func (c *Client) ZRem(ctx context.Context, keys ...string) error {
	cmd := "zrem"
	args := make([]interface{}, 0, 1+len(keys))

	args = append(args, cmd)
	for _, key := range keys {
		args = append(args, key)
	}

	return c.do(ctx, cmd, func(conn *redisConn) error {
		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		_, err := conn.r.ReadIntReply()

		return err
	})
}

func (c *Client) ZRemRangeByRank(ctx context.Context, key string, start, stop int64) (i int64, err error) {
	cmd := "zremrangebyrank"
	err = c.do(ctx, cmd, func(conn *redisConn) error {
		args := []interface{}{cmd, key, start, stop}
		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		i, err = conn.r.ReadIntReply()

		return err
	})
	return
}

func (c *Client) ZRemRangeByScore(ctx context.Context, key, min, max string) (i int64, err error) {
	cmd := "zremrangebyscore"
	err = c.do(ctx, cmd, func(conn *redisConn) error {
		args := []interface{}{cmd, key, min, max}
		if err := conn.w.WriteArgs(args); err != nil {
			return err
		}

		if err := conn.w.Flush(); err != nil {
			return err
		}

		i, err = conn.r.ReadIntReply()

		return err
	})
	return
}

func (c *Client) Stats() *pool.Stats {
	return c.pool.Stats()
}
