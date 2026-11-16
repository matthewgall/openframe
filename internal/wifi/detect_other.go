//go:build !linux

package wifi

// detect: Wi-Fi management is only implemented for the Linux (Raspberry
// Pi) target; every other platform reports unavailable.
func detect(root string, look func(string) (string, error)) Backend {
	return &unavailableBackend{}
}
