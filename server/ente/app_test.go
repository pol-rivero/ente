package ente

import "testing"

func TestAppValidity(t *testing.T) {
	tests := []struct {
		app             App
		valid           bool
		validCollection bool
	}{
		{Photos, true, true},
		{Locker, true, true},
		{Drive, true, true},
		{Auth, true, false},
		{App("unknown"), false, false},
		{App(""), false, false},
	}
	for _, tt := range tests {
		if got := tt.app.IsValid(); got != tt.valid {
			t.Errorf("%q.IsValid() = %t, want %t", tt.app, got, tt.valid)
		}
		if got := tt.app.IsValidForCollection(); got != tt.validCollection {
			t.Errorf("%q.IsValidForCollection() = %t, want %t", tt.app, got, tt.validCollection)
		}
	}
}
