package anthropic

import (
	goanthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/coldcoder126/rein/chat"
)

// cacheControl returns the cache breakpoint marker, or nil when retention is none.
func cacheControl(retention chat.CacheRetention) *goanthropic.CacheControlEphemeralParam {
	if retention == chat.CacheRetentionNone {
		return nil
	}
	cc := goanthropic.NewCacheControlEphemeralParam()
	if retention == chat.CacheRetentionLong {
		cc.TTL = goanthropic.CacheControlEphemeralTTLTTL1h
	}
	return &cc
}

// markLastBlock puts a cache breakpoint on the last block of the last user message, making the whole history a cacheable prefix.
// Tool definitions and the system prompt come earlier in the prefix and are covered by the breakpoint on the system prompt.
func markLastBlock(msgs []goanthropic.MessageParam, cc *goanthropic.CacheControlEphemeralParam) {
	if cc == nil || len(msgs) == 0 {
		return
	}
	last := msgs[len(msgs)-1]
	if last.Role != goanthropic.MessageParamRoleUser || len(last.Content) == 0 {
		return
	}
	if slot := last.Content[len(last.Content)-1].GetCacheControl(); slot != nil {
		*slot = *cc
	}
}
