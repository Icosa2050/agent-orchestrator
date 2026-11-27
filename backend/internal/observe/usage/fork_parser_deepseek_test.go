package usage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Fork (Icosa2050, FORK.md "Usage seams"): the DeepSeek parser records the
// start of the request that produced each reply, so the fork price book can
// price requests spanning a peak/off-peak boundary by request start.
func TestForkUsageSeamDeepSeekRequestStart(t *testing.T) {
	frames := []string{
		`{"type":"session","version":4,"id":"` + deepseekTestSessionID + `","createdAt":1792333057101,"cwd":"/repo"}` + "\n" +
			// The first attempt failed; the retry (the last frame before the reply) produced it.
			`{"type":"request/context","seq":13,"time":1792333057000,"data":{"provider":"deepseek-account","model":"deepseek-v4-pro"}}` + "\n" +
			`{"type":"request/context","seq":14,"time":1792333058000,"data":{"provider":"deepseek-account","model":"deepseek-v4-pro"}}` + "\n" +
			`{"type":"assistant/message","seq":16,"time":1792333059993,"data":{"turn":1,"step":1,"message":{"source":{"provider":"deepseek-account","model":"deepseek-v4-pro"},"id":"m1"},"usage":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":2,"cacheWriteTokens":0}}}` + "\n",
		// No request frame before this reply: it must not inherit seq 14's start.
		`{"type":"assistant/message","seq":20,"time":1792333070000,"data":{"turn":1,"step":2,"message":{"source":{"provider":"deepseek-account","model":"deepseek-v4-pro"},"id":"m2"},"usage":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":0,"cacheWriteTokens":0}}}` + "\n" +
			`{"type":"request/context","seq":21,"time":1792333071000,"data":{}}` + "\n" +
			`{"type":"assistant/message","seq":24,"time":1792333080000,"data":{"turn":2,"step":1,"message":{"source":{"provider":"deepseek-account","model":"deepseek-flash"},"id":"m3"},"usage":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":0,"cacheWriteTokens":0}}}` + "\n",
	}
	path := filepath.Join(t.TempDir(), "--repo--", deepseekTestSessionID, "session.v4.jsonl.zstd")
	writeDeepSeekTestLog(t, path, frames)
	source := usageSource(domain.UsageSourceDeepSeekSession)
	source.NativeRootID = deepseekTestSessionID

	starts := func(after int64) []string {
		t.Helper()
		records, err := readDeepSeekReplies(context.Background(), path, deepseekTestSessionID, after, 10)
		mustNoError(t, err)
		result := parseRecords(source, records, records[len(records)-1].Offset, time.Now())
		mustNoError(t, result.err)
		out := make([]string, 0, len(result.Events))
		for _, event := range result.Events {
			var fields struct {
				RequestStartedAt string `json:"requestStartedAt"`
				InputTokens      int64  `json:"inputTokens"`
			}
			mustNoError(t, json.Unmarshal([]byte(event.ProviderUsageJSON), &fields))
			if fields.InputTokens != 10 {
				t.Fatalf("provider usage lost its token fields: %s", event.ProviderUsageJSON)
			}
			out = append(out, fields.RequestStartedAt)
		}
		return out
	}
	want := []string{
		time.UnixMilli(1792333058000).UTC().Format(time.RFC3339Nano),
		"",
		time.UnixMilli(1792333071000).UTC().Format(time.RFC3339Nano),
	}
	got := starts(0)
	if len(got) != len(want) {
		t.Fatalf("starts = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("starts = %q, want %q", got, want)
		}
	}
	// An incremental read after seq 20 still sees the request frame that
	// precedes the next reply.
	if got := starts(20); len(got) != 1 || got[0] != want[2] {
		t.Fatalf("incremental starts = %q, want [%q]", got, want[2])
	}
}
