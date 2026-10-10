# Minimal, pinned Tesseract image for MyOGChat image OCR (feature 2.5).
#
# Built by deploy/ocr-setup.sh and tagged `myogchat-ocr` by default; the server
# runs `tesseract` in this image via `docker run` (MYOGCHAT_OCR_IMAGE). Keep the
# base pinned for reproducibility.
#
# Package names verified against the Alpine v3.20 community repo (aarch64 and
# x86_64): `tesseract-ocr` (binary) and `tesseract-ocr-data-eng` (English
# traineddata), both 5.3.4-r0.
FROM alpine:3.20

RUN apk add --no-cache tesseract-ocr tesseract-ocr-data-eng

# OCR runs as an unprivileged user (65534:65534) on a read-only rootfs: the
# image is read from stdin and the text written to stdout, so no writable layer
# is needed beyond the /tmp tmpfs the server mounts.
CMD ["tesseract", "--version"]
