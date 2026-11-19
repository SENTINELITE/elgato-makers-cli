package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/listing"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/release"
)

func TestListingStatusHumanAndJSONOffline(t *testing.T) {
	dir, cfg, file := listingFixture(t)
	state := filepath.Join(dir, "state")
	opts := Options{ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
		t.Fatal("offline status connected")
		return nil, nil
	}}
	common := []string{"--config", cfg, "--state-dir", state}
	args := func(extra ...string) []string { return append(append([]string{}, common...), extra...) }
	prepared, _, err := command(t, opts, args("--minify", "listing", "prepare", "--file", file)...)
	if err != nil {
		t.Fatal(err)
	}
	var p listing.Plan
	if err := json.Unmarshal([]byte(prepared), &p); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := command(t, opts, args("listing", "status", p.ID)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Listing: Demo", "Local state: prepared", "Media: 0/5 complete", "Remaining or blocked:", "maker listing show"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q: %s", s, out)
		}
	}
	for _, s := range []string{"\"prepared\"", listingTestDescription, p.Prepared.Digest, dir} {
		if strings.Contains(out, s) {
			t.Errorf("human view leaked %q", s)
		}
	}
	if !strings.Contains(stderr, "Success:") {
		t.Fatal(stderr)
	}
	for _, flag := range []string{"--json", "--minify"} {
		got, _, err := command(t, opts, args(flag, "listing", "status", p.ID)...)
		if err != nil {
			t.Fatal(err)
		}
		var actual listing.Plan
		if err := json.Unmarshal([]byte(got), &actual); err != nil {
			t.Fatal(err)
		}
		canonical, _ := json.Marshal(p)
		actualJSON, _ := json.Marshal(actual)
		if !bytes.Equal(canonical, actualJSON) {
			t.Fatal("status JSON differs from saved plan")
		}
		if flag == "--minify" && strings.Count(got, "\n") != 1 {
			t.Fatal("minify not compact")
		}
	}
}

func TestListingStatusReviewAndFailure(t *testing.T) {
	no := false
	p := listing.Plan{ID: "id", State: listing.Submitted, DetailsState: listing.Done, PackageState: listing.Done, Prepared: listing.Prepared{Config: listing.Config{Name: "Demo", AutoPublish: &no, Pricing: listing.Pricing{Type: "paid", Amount: "2.99", Currency: "USD"}}}}
	for _, status := range []string{"PENDING_APPROVAL", "PUBLISHED"} {
		var b bytes.Buffer
		if err := writeListingStatus(&b, p, &release.Plan{RemoteStatus: status}, false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(b.String(), "Marketplace review status: "+status) {
			t.Fatal(b.String())
		}
	}
	var b bytes.Buffer
	if err := writeListingStatus(&b, p, &release.Plan{State: release.SubmissionUnknown, RemoteStatus: "PENDING_APPROVAL"}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "current status unknown") || !strings.Contains(b.String(), "maker listing status id") || strings.Contains(b.String(), "listing submit") {
		t.Fatal(b.String())
	}
	a := &app{o: Options{Out: &b}, stateDir: t.TempDir()}
	b.Reset()
	if err := a.listingStatusResult(p, errors.New("unavailable")); err == nil || !strings.Contains(b.String(), "current Marketplace status unknown") {
		t.Fatalf("error summary: %v %s", err, b.String())
	}
}

func TestListingStatusReviewSubmissionCheckpoint(t *testing.T) {
	p := listing.Plan{ID: "id", State: listing.Uploaded, DetailsState: listing.Done, PackageState: listing.Done, Media: []listing.MediaCheckpoint{{State: listing.Done}}}
	for _, tc := range []struct {
		name   string
		parent string
		child  *release.Plan
		want   string
		next   bool
	}{
		{"unknown", listing.Uploaded, &release.Plan{State: release.SubmissionUnknown}, "Remaining or blocked: review submission [unknown]", true},
		{"inflight", listing.Uploaded, &release.Plan{State: listing.Inflight}, "Remaining or blocked: review submission [unknown]", true},
		{"not submitted", listing.Uploaded, &release.Plan{State: release.Uploaded}, "Remaining or blocked: review submission [not submitted]", false},
		{"submitted", listing.Submitted, &release.Plan{State: release.Submitted}, "Remaining or blocked: none", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.State = tc.parent
			var b bytes.Buffer
			if err := writeListingStatus(&b, p, tc.child, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(b.String(), tc.want) {
				t.Fatal(b.String())
			}
			if tc.next && (!strings.Contains(b.String(), "maker listing status id") || strings.Contains(b.String(), "maker listing submit")) {
				t.Fatal(b.String())
			}
		})
	}
}
