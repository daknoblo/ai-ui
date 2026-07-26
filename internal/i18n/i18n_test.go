package i18n

import "testing"

// TestCatalogsHaveSameKeys guards against half-translated releases: every key
// must exist in every supported language.
func TestCatalogsHaveSameKeys(t *testing.T) {
	for lang, msgs := range catalog {
		for key := range catalog[EN] {
			if _, ok := msgs[key]; !ok {
				t.Errorf("language %q is missing key %q", lang, key)
			}
		}
		for key := range msgs {
			if _, ok := catalog[EN][key]; !ok {
				t.Errorf("language %q has unknown key %q (not present in %q)", lang, key, EN)
			}
		}
	}
}

// TestNormalize verifies that unknown input falls back to the default language.
func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"de": DE, "DE": DE, " Deutsch ": DE,
		"en": EN, "English": EN,
		"": Default, "klingon": Default,
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTFallsBackToEnglish ensures a missing translation degrades gracefully
// instead of rendering an empty string.
func TestTFallsBackToEnglish(t *testing.T) {
	if got := T(DE, "action.save"); got != "Speichern" {
		t.Errorf("T(de, action.save) = %q", got)
	}
	if got := T(DE, "does.not.exist"); got != "does.not.exist" {
		t.Errorf("unknown key should return the key itself, got %q", got)
	}
	if got := T(EN, "upload.added_many", 3); got != "3 documents added." {
		t.Errorf("formatting failed: %q", got)
	}
}

// TestGroupThousands checks the locale-specific thousands separator.
func TestGroupThousands(t *testing.T) {
	cases := []struct {
		lang string
		in   int64
		want string
	}{
		{EN, 1234567, "1,234,567"},
		{DE, 1234567, "1.234.567"},
		{EN, 999, "999"},
		{EN, -12345, "-12,345"},
		{DE, 0, "0"},
	}
	for _, c := range cases {
		if got := GroupThousands(c.lang, c.in); got != c.want {
			t.Errorf("GroupThousands(%q, %d) = %q, want %q", c.lang, c.in, got, c.want)
		}
	}
}
