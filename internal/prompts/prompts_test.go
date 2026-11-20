package prompts

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maclovin/thehackerdispatch/internal/audit"
	"github.com/maclovin/thehackerdispatch/internal/store/storetest"
)

func TestVersions(t *testing.T) {
	pool := storetest.New(t)
	ctx := context.Background()
	st := New(pool)
	actor := audit.Actor{Label: "admin"}
	const security = 1

	global, section, err := st.For(ctx, security)
	if err != nil || global != DefaultGlobal || section != "" {
		t.Fatalf("before any save: %q, %q, %v", global, section, err)
	}

	if _, err := st.Save(ctx, Global, "  ", actor); !errors.Is(err, ErrEmptyGlobal) {
		t.Errorf("empty global prompt: %v", err)
	}
	if _, err := st.Save(ctx, security, strings.Repeat("x", MaxLength+1), actor); !errors.Is(err, ErrTooLong) {
		t.Errorf("long prompt: %v", err)
	}
	if saved, err := st.Save(ctx, Global, "Global v1\r\nsecond line", actor); !saved || err != nil {
		t.Fatalf("save global: %v %v", saved, err)
	}
	if saved, _ := st.Save(ctx, Global, "Global v1\nsecond line\n", actor); saved {
		t.Error("saving the same text again created a version")
	}
	st.Save(ctx, Global, "Global v2", actor)
	st.Save(ctx, security, "Security v1", actor)

	global, section, _ = st.For(ctx, security)
	if global != "Global v2" || section != "Security v1" {
		t.Errorf("For(security) = %q, %q", global, section)
	}
	if _, section, _ := st.For(ctx, 2); section != "" {
		t.Errorf("a section without a prompt got %q", section)
	}

	history, err := st.History(ctx, Global)
	if err != nil || len(history) != 2 || history[0].Version != 2 || history[1].Body != "Global v1\nsecond line" {
		t.Fatalf("history: %+v %v", history, err)
	}
	scope, err := st.Restore(ctx, history[1].ID, actor)
	if err != nil || scope != Global {
		t.Fatalf("restore: %d %v", scope, err)
	}
	current, _ := st.Current(ctx, Global)
	if current.Version != 3 || current.Body != "Global v1\nsecond line" || current.Author != "admin" {
		t.Errorf("after restore: %+v", current)
	}

	if saved, err := st.Save(ctx, security, "", actor); !saved || err != nil {
		t.Errorf("clear section prompt: %v %v", saved, err)
	}
	if _, section, _ := st.For(ctx, security); section != "" {
		t.Errorf("cleared section prompt is %q", section)
	}
}
