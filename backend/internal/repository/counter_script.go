package repository

import "github.com/redis/go-redis/v9"

// counterIncrScript 原子性地增加计数并在首次写入时设置过期时间（滑动窗口计数器）
var counterIncrScript = redis.NewScript(`
	local key = KEYS[1]
	local ttl = tonumber(ARGV[1])

	local count = redis.call('INCR', key)
	if count == 1 then
		redis.call('EXPIRE', key, ttl)
	end

	return count
`)
