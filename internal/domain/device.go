package domain

import "fmt"

// ValidateSource checks a device UDID wherever it is used — HTTP parameter,
// object-store namespace, catalog key: 1..64 ASCII alphanumerics plus dashes.
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
