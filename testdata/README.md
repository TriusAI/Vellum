# test assets

`scan_page.png` — a rendered scan stand-in (DejaVu Sans text on white,
1654×2339). It was generated once at dev time by the retired Python
version's fixture generator (`PIL` + DejaVu Sans); it is committed as a
static asset because the e2e test wraps it into an image-only PDF to
exercise the OCR path. Regenerating it is not part of the normal build.