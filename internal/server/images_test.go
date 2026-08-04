package server

import "testing"

// TestImageMarkdown makes sure the prompt cannot break out of the Markdown
// image syntax.
func TestImageMarkdown(t *testing.T) {
	got := imageMarkdown(7, "a [cat](evil) on\na roof")
	want := "![a catevil on a roof](/images/7)"
	if got != want {
		t.Errorf("imageMarkdown = %q, want %q", got, want)
	}
}

// TestImageParam falls back to the first allowed value for unknown input.
func TestImageParam(t *testing.T) {
	if got := imageParam("LOW", imageQualities); got != "low" {
		t.Errorf("imageParam = %q, want low", got)
	}
	if got := imageParam("gigantic", imageSizes); got != imageSizes[0] {
		t.Errorf("imageParam = %q, want the fallback %q", got, imageSizes[0])
	}
}
