package provider

import (
	"bytes"
	"image"
	"image/png"
	"slices"
	"strings"
	"testing"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// mediaRec is a media record of typ with a body of n repeated bytes.
func mediaRec(typ, id string, n int) statefile.Record {
	return statefile.Record{
		Header: statefile.Header{Kind: statefile.Message, Type: typ, ID: id},
		Body:   bytes.Repeat([]byte("x"), n),
	}
}

// pngRec is a valid PNG image record of w by h pixels.
func pngRec(t *testing.T, id string, w, h int) statefile.Record {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return statefile.Record{
		Header: statefile.Header{Kind: statefile.Message, Type: "image/png", ID: id},
		Body:   buf.Bytes(),
	}
}

// smallLimits are tight enough to exercise every limit without large bodies.
var smallLimits = Limits{Side: 100, Pixels: 100 * 100, Bytes: 500, Count: 3, Request: 1000}

// A non-image media record under every limit passes through unchanged: the
// fix adds a cap, not a blanket degradation.
func TestFitNonImageMediaUnderLimits(t *testing.T) {
	r := mediaRec("application/pdf", "pdf-under", 100)
	out := fit("google", []statefile.Record{r}, smallLimits, geminiMedia)
	if len(out) != 1 || out[0].Type != "application/pdf" || !bytes.Equal(out[0].Body, r.Body) {
		t.Fatalf("pdf under limits changed: %+v", out)
	}
}

// A non-image record over Bytes degrades to text with the per-record message.
func TestFitNonImageMediaOverBytes(t *testing.T) {
	r := mediaRec("application/pdf", "pdf-over-bytes", smallLimits.Bytes+1)
	out := fit("google", []statefile.Record{r}, smallLimits, geminiMedia)
	if len(out) != 1 || out[0].Type != "text/plain" {
		t.Fatalf("over-Bytes pdf not degraded: %+v", out)
	}
	if !strings.Contains(string(out[0].Body), "[media not sent:") {
		t.Fatalf("over-Bytes pdf missing notice: %q", out[0].Body)
	}
}

// A per-record over-Bytes record degrades without spending the budget, so a
// later record under Bytes is still sent.
func TestFitNonImageMediaOverBytesDoesNotSetFull(t *testing.T) {
	big := mediaRec("application/pdf", "pdf-big", smallLimits.Bytes+1)
	small := mediaRec("application/pdf", "pdf-small", 100)
	out := fit("google", []statefile.Record{big, small}, smallLimits, geminiMedia)
	if out[0].Type != "text/plain" {
		t.Fatalf("over-Bytes pdf not degraded: %+v", out[0])
	}
	if out[1].Type != "application/pdf" {
		t.Fatalf("small pdf after over-Bytes pdf degraded: %+v", out[1])
	}
}

// Once Count is reached, the record that crosses it and every record after it
// degrade with the session-full message.
func TestFitNonImageMediaOverCount(t *testing.T) {
	recs := []statefile.Record{
		mediaRec("application/pdf", "pdf-c1", 100),
		mediaRec("application/pdf", "pdf-c2", 100),
		mediaRec("application/pdf", "pdf-c3", 100),
		mediaRec("application/pdf", "pdf-c4", 100),
	}
	out := fit("google", recs, smallLimits, geminiMedia)
	for i, r := range out {
		switch {
		case i < 3:
			if r.Type != "application/pdf" {
				t.Fatalf("record %d under Count degraded: %+v", i, r)
			}
		default:
			if r.Type != "text/plain" || !strings.Contains(string(r.Body), "media limit") {
				t.Fatalf("record %d over Count not degraded: %+v", i, r)
			}
		}
	}
}

// Once full, every later non-image record degrades, even one under Bytes.
func TestFitFullIsSticky(t *testing.T) {
	recs := []statefile.Record{
		mediaRec("application/pdf", "pdf-s1", 400),
		mediaRec("application/pdf", "pdf-s2", 400),
		mediaRec("application/pdf", "pdf-s3", 400), // triggers full
		mediaRec("application/pdf", "pdf-s4", 100), // under Bytes but full
	}
	out := fit("google", recs, smallLimits, geminiMedia)
	if out[0].Type != "application/pdf" || out[1].Type != "application/pdf" {
		t.Fatalf("first two degraded: %+v", out)
	}
	if out[2].Type != "text/plain" || out[3].Type != "text/plain" {
		t.Fatalf("full not sticky: %+v", out)
	}
}

// Images are still fitted, not treated as non-image media.
func TestFitImagesStillProcessed(t *testing.T) {
	r := pngRec(t, "img-ok", 2, 2)
	out := fit("google", []statefile.Record{r}, smallLimits, geminiMedia)
	if len(out) != 1 {
		t.Fatalf("image record lost: %+v", out)
	}
	if !slices.Contains(images, out[0].Type) {
		t.Fatalf("image type changed to %q", out[0].Type)
	}
}

// With the images list (not geminiMedia), non-image media passes through
// unchanged: only Gemini inlines non-image media, so only Gemini caps it.
func TestFitNonGeminiPassesNonImageThrough(t *testing.T) {
	r := mediaRec("application/pdf", "pdf-other", 100)
	out := fit("anthropic", []statefile.Record{r}, smallLimits, images)
	if len(out) != 1 || out[0].Type != "application/pdf" || !bytes.Equal(out[0].Body, r.Body) {
		t.Fatalf("pdf changed under non-gemini media list: %+v", out)
	}
}

// Every non-image type Gemini inlines is capped, not just PDF.
func TestFitAllGeminiMediaTypesCapped(t *testing.T) {
	for _, typ := range []string{"application/pdf", "audio/mpeg", "audio/aiff", "video/mp4", "video/webm"} {
		t.Run(typ, func(t *testing.T) {
			r := mediaRec(typ, "over-"+typ, smallLimits.Bytes+1)
			out := fit("google", []statefile.Record{r}, smallLimits, geminiMedia)
			if out[0].Type != "text/plain" {
				t.Fatalf("over-Bytes %s not degraded: %+v", typ, out[0])
			}
		})
	}
}

// Compile degrades a PDF over gemLimits.Bytes to text, not inlineData, and the
// body stays small: the session is not bricked by a single large media file.
func TestGeminiCompileDegradesOverBytesPDF(t *testing.T) {
	r := statefile.Record{
		Header: statefile.Header{Kind: statefile.Message, Type: "application/pdf", ID: "pdf-big", Via: "test"},
		Body:   bytes.Repeat([]byte("x"), gemLimits.Bytes+1),
	}
	req := Request{Context: []statefile.Record{r}}
	body, _, err := gemini{}.Compile(req)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(`"inlineData"`)) {
		t.Fatalf("over-Bytes pdf inlined: %s", body[:200])
	}
	if !bytes.Contains(body, []byte("[media not sent:")) {
		t.Fatalf("over-Bytes pdf not degraded: %s", body)
	}
	if len(body) > 1<<20 {
		t.Fatalf("over-Bytes pdf body too large: %d bytes", len(body))
	}
}
