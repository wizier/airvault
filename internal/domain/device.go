package domain

import "fmt"

// A UDID is also an object-store namespace and catalog key, so it is checked
// the same way wherever it enters.
func ValidateSource(source string) error {
	if len(source) == 0 || len(source) > 64 {
		return fmt.Errorf("source must be 1..64 ASCII characters")
	}
	hasAlphanumeric := false
	for i := range len(source) {
		char := source[i]
		switch {
		case char >= '0' && char <= '9', char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z':
			hasAlphanumeric = true
		case char == '-':
		default:
			return fmt.Errorf("source contains an invalid character")
		}
	}
	if !hasAlphanumeric {
		return fmt.Errorf("source must contain an alphanumeric character")
	}
	return nil
}
