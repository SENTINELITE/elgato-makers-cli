package maker

import (
	"context"
	"errors"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"strings"
)

// CreationReceipt binds a successful, acknowledged creation to its original
// identity. Callers must recover it only from their durable creation journal.
type CreationReceipt struct {
	Target      model.Target `json:"target"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
}
type creationReceiptKey struct{}

func WithCreationReceipt(ctx context.Context, receipt CreationReceipt) (context.Context, error) {
	t := receipt.Target
	if safeID(t.OrgID) != nil || safeID(t.ProductID) != nil || safeID(t.VariantID) != nil || t.UUID == "" || receipt.Name == "" || receipt.Description == "" {
		return nil, errors.New("invalid acknowledged creation receipt")
	}
	return context.WithValue(ctx, creationReceiptKey{}, receipt), nil
}

func listingVariantIdentity(ctx context.Context, target model.Target, product, variant map[string]any, resolve func(any) any) error {
	if target.UUID == "" {
		return nil
	}
	raw, exists := variant["streamdeckid"]
	uuid, valid := resolve(raw).(string)
	if uuid != "" {
		if uuid != target.UUID {
			return errors.New("listing variant identity mismatch")
		}
		return nil
	}
	if !exists || (resolve(raw) != nil && !valid) {
		return errors.New("listing variant identity schema is not recognized")
	}
	receipt, ok := ctx.Value(creationReceiptKey{}).(CreationReceipt)
	if !ok || receipt.Target.OrgID != target.OrgID || receipt.Target.ProductID != target.ProductID || receipt.Target.VariantID != target.VariantID || receipt.Target.UUID != target.UUID {
		return errors.New("listing variant identity mismatch; missing UUID requires acknowledged creation receipt")
	}
	pid, _ := resolve(product["id"]).(string)
	vid, _ := resolve(variant["id"]).(string)
	name, _ := resolve(product["name"]).(string)
	description, _ := resolve(product["description"]).(string)
	status, _ := resolve(product["status"]).(string)
	versions, recognized := resolve(variant["versions"]).([]any)
	if pid != target.ProductID || vid != target.VariantID || name != receipt.Name || !model.SameListingDescription(description, receipt.Description) || !strings.EqualFold(status, "DRAFT") || !recognized || len(versions) != 0 {
		return errors.New("provisional listing identity differs from acknowledged creation receipt")
	}
	return nil
}
