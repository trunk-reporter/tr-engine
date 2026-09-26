package database

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/snarg/tr-engine/internal/auth"
)

// TestNilPrincipalIsAnError checks §7.2: every query function behind a
// Restricted = Enforced route refuses a nil principal, before it touches the
// database (this DB has no pool, so any query would panic). A missing
// principal never means "unrestricted".
func TestNilPrincipalIsAnError(t *testing.T) {
	db := &DB{}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"ListCalls":   func() error { _, _, err := db.ListCalls(ctx, nil, CallFilter{}); return err },
		"GetCallByID": func() error { _, err := db.GetCallByID(ctx, nil, 1); return err },
		"ListCallGroups": func() error {
			_, _, err := db.ListCallGroups(ctx, nil, CallGroupFilter{})
			return err
		},
		"GetCallGroupByID": func() error { _, _, err := db.GetCallGroupByID(ctx, nil, 1); return err },
		"GetCallAudioPath": func() error { _, _, err := db.GetCallAudioPath(ctx, nil, 1); return err },
		"GetCallFrequencies": func() error {
			_, err := db.GetCallFrequencies(ctx, nil, 1)
			return err
		},
		"GetCallTransmissions": func() error {
			_, err := db.GetCallTransmissions(ctx, nil, 1)
			return err
		},
		"GetSystemByID":        func() error { _, err := db.GetSystemByID(ctx, nil, 1); return err },
		"ListSystemsWithSites": func() error { _, err := db.ListSystemsWithSites(ctx, nil); return err },
		"GetSiteByID":          func() error { _, err := db.GetSiteByID(ctx, nil, 1); return err },
		"GetTalkgroupByComposite": func() error {
			_, err := db.GetTalkgroupByComposite(ctx, nil, 1, 100)
			return err
		},
		"FindTalkgroupSystems": func() error { _, err := db.FindTalkgroupSystems(ctx, nil, 100); return err },
		"ListTalkgroups": func() error {
			_, _, err := db.ListTalkgroups(ctx, nil, TalkgroupFilter{})
			return err
		},
		"SearchTalkgroupDirectory": func() error {
			_, _, err := db.SearchTalkgroupDirectory(ctx, nil, TalkgroupDirectoryFilter{})
			return err
		},
		"GetPrimaryTranscription": func() error {
			_, err := db.GetPrimaryTranscription(ctx, nil, 1)
			return err
		},
		"ListTranscriptionsByCall": func() error {
			_, err := db.ListTranscriptionsByCall(ctx, nil, 1)
			return err
		},
		"SearchTranscriptions": func() error {
			_, _, err := db.SearchTranscriptions(ctx, nil, "engine", TranscriptionSearchFilter{})
			return err
		},
		"GetBatchTranscriptions": func() error {
			_, err := db.GetBatchTranscriptions(ctx, nil, nil)
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrNoPrincipal) {
			t.Errorf("%s with a nil principal: err = %v, want ErrNoPrincipal", name, err)
		}
	}
}

func TestAllowedPatchedTgids(t *testing.T) {
	restricted := &auth.Principal{Kind: auth.KindKey, Scopes: auth.Scopes{auth.ScopeListen},
		Restrictions: []auth.Restriction{{Systems: []int{1}, ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 300}}}}}
	nothing := &auth.Principal{Kind: auth.KindTicket, Scopes: auth.Scopes{auth.ScopeListen},
		Restrictions: []auth.Restriction{{}}}
	patched := []int32{200, 300, 0}
	for _, tc := range []struct {
		name     string
		p        *auth.Principal
		systemID int
		want     []int32
	}{
		{"unrestricted", auth.Internal, 2, []int32{200, 300, 0}},
		{"restricted, allowed system", restricted, 1, []int32{200}},
		{"restricted, other system", restricted, 2, nil},
		{"allows nothing", nothing, 1, nil},
	} {
		got := allowedPatchedTgids(tc.p, tc.systemID, slices.Clone(patched))
		if !slices.Equal(got, tc.want) || (tc.want == nil) != (got == nil) {
			t.Errorf("%s: allowedPatchedTgids = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestTranscriptionSourceStatus pins the source → transcription_status
// mapping of InsertTranscription: every source the transcriptions.source
// CHECK accepts maps to a status that the calls/call_groups CHECK accepts.
func TestTranscriptionSourceStatus(t *testing.T) {
	statuses := map[string]bool{"none": true, "auto": true, "reviewed": true, "verified": true, "excluded": true, "empty": true}
	for _, source := range []string{"auto", "human", "llm"} {
		if !ValidTranscriptionSource(source) {
			t.Errorf("source %q is not accepted", source)
		}
		if !statuses[transcriptionStatusForSource[source]] {
			t.Errorf("source %q maps to status %q, which the CHECK rejects", source, transcriptionStatusForSource[source])
		}
	}
	for _, source := range []string{"", "robot", "Human", "verified"} {
		if ValidTranscriptionSource(source) {
			t.Errorf("source %q is accepted", source)
		}
		if _, err := (&DB{}).InsertTranscription(context.Background(), &TranscriptionRow{Source: source}); !errors.Is(err, ErrInvalidTranscriptionSource) {
			t.Errorf("InsertTranscription(source %q): err = %v, want ErrInvalidTranscriptionSource", source, err)
		}
	}
	if transcriptionStatusForSource["human"] != "verified" || transcriptionStatusForSource["llm"] != "auto" {
		t.Errorf("status mapping = %v", transcriptionStatusForSource)
	}
}
