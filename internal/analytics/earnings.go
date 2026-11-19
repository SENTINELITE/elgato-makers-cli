package analytics

const (
	// PlatformFeePercent is the assumed Elgato platform fee.
	PlatformFeePercent = 30
	// CreatorSharePercent is the creator's estimated share after the assumed fee.
	CreatorSharePercent = 100 - PlatformFeePercent
)

// EstimatedCreatorRevenue returns the estimated creator share in cents, rounded
// to the nearest cent with half cents rounded away from zero. Negative gross
// amounts retain their sign so refunds reduce the estimate.
func EstimatedCreatorRevenue(grossCents int64) int64 {
	// Split before multiplying so every int64 input is safe from overflow.
	whole := (grossCents / 100) * CreatorSharePercent
	fraction := (grossCents % 100) * CreatorSharePercent
	if fraction >= 0 {
		return whole + (fraction+50)/100
	}
	return whole + (fraction-50)/100
}
