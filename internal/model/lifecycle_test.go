package model

import (
	"errors"
	"testing"
)

func TestVersionMutationReviewAliasesAndUnknownStates(t *testing.T) {
	for _, status := range []string{"review", " pendingReview ", "PENDING_REVIEW", "pending-review", "pending review", "inReview", "IN_REVIEW", "in-review", "in review"} {
		t.Run(status, func(t *testing.T) {
			err := CheckVersionMutation([]Version{{Version: "1", Status: status}}, "variant", "2")
			if !errors.Is(err, ErrVersionInReview) {
				t.Fatalf("expected review gate, got %v", err)
			}
		})
	}
	for _, status := range []string{"", "REVIEWED", "not pending review", "re_view", "approved/published"} {
		t.Run(status, func(t *testing.T) {
			err := CheckVersionMutation([]Version{{Version: "1", Status: status}}, "variant", "2")
			if !errors.Is(err, ErrUnrecognizedVersionLifecycle) {
				t.Fatalf("expected unknown lifecycle gate, got %v", err)
			}
		})
	}
}

func TestVersionMutationAllowsKnownStatesAndIgnoresOtherVariants(t *testing.T) {
	for _, status := range []string{"draft", " Approved ", "PUBLISHED"} {
		t.Run(status, func(t *testing.T) {
			err := CheckVersionMutation([]Version{
				{Version: "1", Status: status, VariantID: "variant"},
				{Version: "2", Status: "review", VariantID: "other"},
				{Version: "2", Status: "ambiguous", VariantID: "other"},
			}, "variant", "2")
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVersionMutationRejectsEquivalentVersionNumbers(t *testing.T) {
	err := CheckVersionMutation([]Version{
		{Version: "2", Status: "review", VariantID: "variant"},
		{Version: "1", Status: "published", VariantID: "variant"},
	}, "variant", "1.0.0.0")
	if !errors.Is(err, ErrDuplicateVersion) {
		t.Fatalf("expected duplicate version gate, got %v", err)
	}
}
