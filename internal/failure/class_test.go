package failure

import "testing"

func TestClassString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		class Class
		want  string
	}{
		{name: "Success", class: Success, want: "success"},
		{name: "Transient", class: Transient, want: "transient"},
		{name: "Capacity", class: Capacity, want: "capacity"},
		{name: "Permanent", class: Permanent, want: "permanent"},
		{name: "Invalid", class: Invalid, want: "invalid"},
		{name: "Unknown", class: Unknown, want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.class.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}

	if got := Class(99).String(); got != Unknown.String() {
		t.Fatalf("unrecognized class String() = %q, want the unknown label %q", got, Unknown.String())
	}
}

func TestClassIsRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		class Class
		want  bool
	}{
		{class: Success, want: false},
		{class: Invalid, want: false},
		{class: Unknown, want: true},
		{class: Transient, want: true},
		{class: Capacity, want: true},
		{class: Permanent, want: true},
	}

	for _, tt := range tests {
		if got := tt.class.IsRetryable(); got != tt.want {
			t.Errorf("Class(%d).IsRetryable() = %t, want %t", tt.class, got, tt.want)
		}
	}
}
