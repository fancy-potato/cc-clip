//go:build !darwin

package daemon

// DarwinNotifier is a no-op on non-darwin platforms.
type DarwinNotifier = NopNotifier

func NewDarwinNotifier() *DarwinNotifier {
	return &DarwinNotifier{}
}
