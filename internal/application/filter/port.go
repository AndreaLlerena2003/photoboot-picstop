package filter

import "context"

// IFilterPort is the output port for image filter operations.
// Application layer owns this interface; infrastructure implements it (DIP).
type IFilterPort interface {
	// ApplyToFile applies the named filter to the JPEG at originalPath and writes
	// the result alongside it as <base>_filtered<ext>. The original is never modified.
	ApplyToFile(ctx context.Context, originalPath, filterName string) (filteredPath string, err error)

	// WrapPreviewChannel returns a new channel that emits filtered JPEG frames from raw.
	// The filterName hint is used as an override; if "" the implementation uses its
	// current active filter. The returned channel closes when raw closes or ctx is done.
	WrapPreviewChannel(ctx context.Context, raw <-chan []byte, filterName string) <-chan []byte

	// ListFilters returns the names of all available filters (sorted).
	ListFilters() []string

	// ActiveFilter returns the name of the currently active filter, or "" if none.
	ActiveFilter() string

	// SetActiveFilter sets the active filter by name.
	// Pass "" to clear the active filter.
	// Returns an error if the name is unknown (and non-empty).
	SetActiveFilter(name string) error
}
