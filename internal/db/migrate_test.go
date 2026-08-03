package db

import "testing"

func TestValidateMigrationVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		version int
		wantErr bool
	}{
		{version: -2, wantErr: true},
		{version: -1},
		{version: 0},
	}
	for _, tt := range tests {
		t.Run("version", func(t *testing.T) {
			t.Parallel()

			if err := validateMigrationVersion(tt.version); (err != nil) != tt.wantErr {
				t.Errorf("validateMigrationVersion(%d) error = %v, wantErr %t", tt.version, err, tt.wantErr)
			}
		})
	}
}
