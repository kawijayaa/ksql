package runtime

import "testing"

func TestScalarText(t *testing.T) {
	tenth := 0.1
	for _, tc := range []struct {
		value any
		want  any
	}{
		{32.15620994567871, "32.15620994567871"}, {100.0, "100"}, {tenth + 0.2, "0.30000000000000004"},
		{1e15, "1E+15"}, {1e14, "100000000000000"}, {123456789012345680000.0, "1.2345678901234568E+20"},
		{0.0001, "0.0001"}, {0.00001, "1E-05"}, {-2.5e-7, "-2.5E-07"}, {1e300, "1E+300"}, {0.0, "0"},
		{int64(-7), "-7"}, {"x", "x"}, {[]byte("y"), "y"}, {[]byte(nil), nil}, {nil, nil},
	} {
		if got := ScalarText(tc.value); got != tc.want {
			t.Errorf("ScalarText(%v) = %v, want %v", tc.value, got, tc.want)
		}
	}
}
