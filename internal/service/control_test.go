package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wizier/airvault/internal/domain"
)

// An invalid icon batch is refused before the device is looked up.
func TestAppIconsValidatesBatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bundleIDs []string
		code      string
	}{
		{"empty list", nil, "bundle_id_required"},
		{"empty id", []string{"com.example.app", ""}, "bundle_id_required"},
		{"over the cap", slices.Repeat([]string{"com.example.app"}, maxAppIconBatch+1), "too_many_bundle_ids"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&Service{}).AppIcons(context.Background(), "udid", tc.bundleIDs)
			var validation *domain.ValidationError
			if !errors.As(err, &validation) || validation.Code != tc.code {
				t.Fatalf("err = %v, want validation code %q", err, tc.code)
			}
		})
	}
}
