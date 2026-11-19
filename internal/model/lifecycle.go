package model

import (
	"errors"
	"fmt"
	"strings"
)

type VersionLifecycle uint8

const (
	VersionLifecycleUnknown VersionLifecycle = iota
	VersionLifecycleDraft
	VersionLifecycleReview
	VersionLifecycleApproved
	VersionLifecyclePublished
)

var (
	ErrDuplicateVersion             = errors.New("version already exists; refusing duplicate release")
	ErrVersionInReview              = errors.New("a version is pending review; refusing release")
	ErrUnrecognizedVersionLifecycle = errors.New("an existing version has an unrecognized lifecycle status; refusing release")
)

// ParseVersionLifecycle recognizes Maker states without modifying wire data.
func ParseVersionLifecycle(raw string) VersionLifecycle {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "draft":
		return VersionLifecycleDraft
	case "review", "pendingreview", "pending_review", "pending-review", "pending review", "inreview", "in_review", "in-review", "in review":
		return VersionLifecycleReview
	case "approved":
		return VersionLifecycleApproved
	case "published":
		return VersionLifecyclePublished
	default:
		return VersionLifecycleUnknown
	}
}

// CheckVersionMutation expects versions from the target product. An explicitly
// different variant is out of scope. An unscoped row can still be relevant.
func CheckVersionMutation(versions []Version, variantID, requestedVersion string) error {
	var lifecycleErr error
	for _, version := range versions {
		if variantID != "" && version.VariantID != "" && version.VariantID != variantID {
			continue
		}
		if SameStreamDeckVersion(version.Version, requestedVersion) {
			return fmt.Errorf("%w: %s", ErrDuplicateVersion, requestedVersion)
		}
		if lifecycleErr != nil {
			continue
		}
		switch ParseVersionLifecycle(version.Status) {
		case VersionLifecycleReview:
			lifecycleErr = ErrVersionInReview
		case VersionLifecycleUnknown:
			lifecycleErr = ErrUnrecognizedVersionLifecycle
		}
	}
	return lifecycleErr
}
