package provider

import (
	"strings"
	"testing"
	"time"
)

func TestMakeClientToken(t *testing.T) {
	tok := makeClientToken("prod-omni-AWS Workers Large-6rlg9q", 0)
	if strings.Contains(tok, " ") {
		t.Fatalf("token still has spaces: %q", tok)
	}
	if len(tok) > 64 {
		t.Fatalf("token longer than 64: %d %q", len(tok), tok)
	}
	if makeClientToken("abc", 1) == makeClientToken("abc", 2) {
		t.Fatal("attempt must change the token")
	}
}

func TestMissingInstanceShouldRetry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	grace := 2 * time.Minute

	t.Run("zero createdAt stamps and retries", func(t *testing.T) {
		retry, stamp := missingInstanceShouldRetry(time.Time{}, now, grace)
		if !retry || !stamp {
			t.Fatalf("retry=%v stamp=%v, want retry and stamp", retry, stamp)
		}
	})

	t.Run("recent create retries without stamping", func(t *testing.T) {
		retry, stamp := missingInstanceShouldRetry(now.Add(-30*time.Second), now, grace)
		if !retry || stamp {
			t.Fatalf("retry=%v stamp=%v, want retry and no stamp", retry, stamp)
		}
	})

	t.Run("after grace does not retry", func(t *testing.T) {
		retry, stamp := missingInstanceShouldRetry(now.Add(-2*time.Minute), now, grace)
		if retry || stamp {
			t.Fatalf("retry=%v stamp=%v, want replace", retry, stamp)
		}
	})
}
