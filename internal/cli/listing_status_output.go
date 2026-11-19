package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/SENTINELITE/elgato-makers-cli/internal/listing"
	"github.com/SENTINELITE/elgato-makers-cli/internal/release"
)

// listingStatusResult keeps the structured plan unchanged for scripts. The
// human view reads only the local child journal already updated by Status.
func (a *app) listingStatusResult(p listing.Plan, operationErr error) error {
	if a.json || a.minify {
		if err := a.output(p); err != nil {
			return errors.Join(operationErr, err)
		}
	} else {
		dir := a.stateDir
		if dir == "" {
			dir = filepath.Join(filepath.Dir(a.configPath), "releases")
		}
		var child *release.Plan
		if p.ReleasePlanID != "" {
			if rp, err := release.NewManager(filepath.Join(dir, "listings", "releases"), nil).Load(p.ReleasePlanID); err == nil {
				child = &rp
			}
		}
		if err := writeListingStatus(a.o.Out, p, child, operationErr != nil); err != nil {
			return errors.Join(operationErr, err)
		}
	}
	if operationErr != nil {
		return operationErr
	}
	a.completion = fmt.Sprintf("Success: listing %s status read.", p.ID)
	return nil
}

func writeListingStatus(w io.Writer, p listing.Plan, child *release.Plan, failed bool) error {
	unknown := func(s string) string {
		if s == "" {
			return "unknown"
		}
		return s
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Listing: %s (%s)\nVersion: %s\nLocal state: %s\n", unknown(p.Prepared.Config.Name), p.ID, unknown(p.Prepared.Package.Manifest.Version), unknown(p.State))
	fmt.Fprintf(&b, "Remote product: %s\nRemote variant: %s\n", unknown(p.Target.ProductID), unknown(p.Target.VariantID))
	fmt.Fprintf(&b, "Plugin UUID: %s\n", unknown(p.Target.UUID))
	if child != nil && child.State == release.Submitted && child.SubmissionID != "" {
		fmt.Fprintf(&b, "Marketplace version ID: %s\n", child.SubmissionID)
	}
	review := "unknown"
	if child != nil && child.RemoteStatus != "" {
		review = child.RemoteStatus
		if failed {
			review += " (last saved; current status unknown)"
		}
	} else if p.State == listing.Submitted {
		review = "submitted; current Marketplace status unknown"
	}
	fmt.Fprintf(&b, "Marketplace review status: %s\n", review)
	price := p.Prepared.Config.Pricing
	fmt.Fprintf(&b, "Reviewed price: %s", unknown(price.Type))
	if price.Amount != "" {
		fmt.Fprintf(&b, " %s %s", price.Amount, price.Currency)
	}
	autopublish := "unknown"
	if p.Prepared.Config.AutoPublish != nil {
		autopublish = fmt.Sprint(*p.Prepared.Config.AutoPublish)
	}
	fmt.Fprintf(&b, "\nAuto-publish: %s\n", autopublish)
	completed, remaining := []string{}, []string{}
	add := func(name, state string) {
		if state == listing.Done {
			completed = append(completed, name)
		} else {
			remaining = append(remaining, fmt.Sprintf("%s [%s]", name, unknown(state)))
		}
	}
	if p.EarlyDetailsState != "" {
		add("initial details", p.EarlyDetailsState)
	}
	add("listing details", p.DetailsState)
	add("package", p.PackageState)
	mediaDone := 0
	for _, m := range p.Media {
		if m.State == listing.Done {
			mediaDone++
		}
	}
	fmt.Fprintf(&b, "Media: %d/%d complete\n", mediaDone, len(p.Media))
	if mediaDone != len(p.Media) {
		remaining = append(remaining, "media")
	}
	if p.State != listing.Retired && p.State != listing.Discarded {
		switch {
		case child != nil && (child.State == release.SubmissionUnknown || child.State == listing.Inflight):
			remaining = append(remaining, "review submission [unknown]")
		case p.State == listing.Submitted || (child != nil && child.State == release.Submitted):
			completed = append(completed, "review submission")
		case p.State == listing.Uploaded:
			remaining = append(remaining, "review submission [not submitted]")
		}
	}
	if len(completed) == 0 {
		completed = append(completed, "none")
	}
	if len(remaining) == 0 {
		remaining = append(remaining, "none")
	}
	fmt.Fprintf(&b, "Completed: %s\nRemaining or blocked: %s\n", strings.Join(completed, ", "), strings.Join(remaining, ", "))
	uncertain := failed || p.State == listing.Unknown || p.State == listing.Inflight || p.EarlyDetailsState == listing.Unknown || p.EarlyDetailsState == listing.Inflight || p.DetailsState == listing.Unknown || p.DetailsState == listing.Inflight || p.PackageState == listing.Unknown || p.PackageState == listing.Inflight || (child != nil && (child.State == release.SubmissionUnknown || child.State == listing.Inflight))
	for _, m := range p.Media {
		uncertain = uncertain || m.State == listing.Unknown || m.State == listing.Inflight
	}
	if uncertain {
		fmt.Fprintf(&b, "Next: inspect with maker listing status %s; resolve any reported error before further action.\n", p.ID)
	} else {
		fmt.Fprintf(&b, "Full saved plan: maker listing show %s\n", p.ID)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
