package korphan

import "testing"

func TestIgnoreAnnotationState(t *testing.T) {
	cases := []struct {
		name       string
		annos      map[string]string
		wantState  ignoreState
		wantReason string
	}{
		{"absent", nil, ignoreNone, ""},
		{"other annotation only", map[string]string{"foo": "bar"}, ignoreNone, ""},
		{"non-empty reason exempts", map[string]string{IgnoreAnnotation: "rotated out-of-band"}, ignoreActive, "rotated out-of-band"},
		{"reason is trimmed", map[string]string{IgnoreAnnotation: "  hi  "}, ignoreActive, "hi"},
		{"empty value not honored", map[string]string{IgnoreAnnotation: ""}, ignoreEmpty, ""},
		{"whitespace value not honored", map[string]string{IgnoreAnnotation: "   "}, ignoreEmpty, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, reason := ignoreAnnotationState(c.annos)
			if st != c.wantState {
				t.Errorf("state = %d, want %d", st, c.wantState)
			}
			if reason != c.wantReason {
				t.Errorf("reason = %q, want %q", reason, c.wantReason)
			}
		})
	}
}
