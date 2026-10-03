// Package testutil generates small PDF fixtures for the e2e test:
// a text-layer ("born-digital") PDF and an image-only ("scanned") PDF.
// It is a deliberate minimal PDF writer — test code only, not part of the
// shipped product.
package testutil

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"image/png"
	"os"
	"strings"
)

func objStr(s string) obj { return obj{data: []byte(s)} }

func streamObj(header string, body []byte) obj {
	data := []byte(fmt.Sprintf("<< %s /Length %d >>\nstream\n", header, len(body)))
	data = append(data, body...)
	data = append(data, "\nendstream"...)
	return obj{data: data}
}

type obj struct {
	data []byte
}

func writePDF(path string, objects []obj, trailerExtra string) error {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.7\n")
	offsets := make([]int, len(objects))
	for i, o := range objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n", i+1)
		buf.Write(o.data)
		buf.WriteString("\nendobj\n")
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R%s >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, trailerExtra, xref)
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func escapePDFText(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)")
	return r.Replace(s)
}

// TextPage is one page of lines for a born-digital PDF.
type TextPage struct {
	Title string
	Lines []string
}

// WriteTextPDF writes a PDF whose pages carry a real text layer, with the
// given title in the Info dictionary.
func WriteTextPDF(path, title string, pages []TextPage) error {
	objects := []obj{
		objStr("<< /Type /Catalog /Pages 2 0 R >>"),
		objStr("<< >>"), // patched after the page list is known
		objStr("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
	}
	kids := []string{}
	for _, p := range pages {
		var b bytes.Buffer
		b.WriteString("BT /F1 11 Tf 14 TL 72 770 Td\n")
		for _, line := range append([]string{p.Title}, p.Lines...) {
			fmt.Fprintf(&b, "(%s) Tj T*\n", escapePDFText(line))
		}
		b.WriteString("ET")
		contentID := 5 + 2*len(kids)
		pageID := contentID - 1
		kids = append(kids, fmt.Sprintf("%d 0 R", pageID))
		objects = append(objects,
			objStr(fmt.Sprintf("<< /Type /Page /Parent 2 0 R "+
				"/MediaBox [0 0 595 842] /Resources << /Font << /F1 3 0 R >> >> "+
				"/Contents %d 0 R >>", contentID)),
			streamObj("", b.Bytes()))
	}
	infoID := len(objects) + 1
	objects[1] = objStr(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>",
		strings.Join(kids, " "), len(pages)))
	objects = append(objects, objStr(fmt.Sprintf(
		"<< /Title (%s) /Producer (vellum-test) >>", escapePDFText(title))))
	objects[0] = objStr("<< /Type /Catalog /Pages 2 0 R >>")
	return writePDF(path, objects, fmt.Sprintf(" /Info %d 0 R", infoID))
}

// WriteImagePDF wraps an image file into an image-only PDF — a stand-in for
// a scanned page with no text layer.
func WriteImagePDF(path, imgPath string) error {
	data, err := os.ReadFile(imgPath)
	if err != nil {
		return err
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	rgb := make([]byte, 0, w*h*3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			rgb = append(rgb, byte(r>>8), byte(g>>8), byte(bl>>8))
		}
	}
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(rgb); err != nil {
		return err
	}
	zw.Close()

	content := "q 595 0 0 842 0 0 cm /Im0 Do Q"
	objects := []obj{
		objStr("<< /Type /Catalog /Pages 2 0 R >>"),
		objStr("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		objStr("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] " +
			"/Resources << /XObject << /Im0 4 0 R >> >> /Contents 5 0 R >>"),
		streamObj("/Type /XObject /Subtype /Image /Width "+
			fmt.Sprint(w)+" /Height "+fmt.Sprint(h)+
			" /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode", zbuf.Bytes()),
		streamObj("", []byte(content)),
	}
	return writePDF(path, objects, "")
}
