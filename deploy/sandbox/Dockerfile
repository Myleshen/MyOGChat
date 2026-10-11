# Prebuilt MyOGChat code-sandbox Python image (feature 5.8).
#
# Built by deploy/sandbox-setup.sh and tagged `myogchat-sandbox-python` by
# default; the server runs `python -` in this image via `docker run`
# (MYOGCHAT_SANDBOX_IMAGE_PYTHON). It is a superset of python:<tag>-slim adding
# only the file-writing libraries an agent needs to produce real documents:
#
#   reportlab   PDF
#   openpyxl    XLSX
#   python-docx DOCX
#   Pillow      images
#   matplotlib  charts
#
# CSV / TXT / JSON / Markdown need no libraries. Keep this list small: a bigger
# image costs disk on the Pi. The base is pinned for reproducibility; it must
# match the server's Python version expectation (3.14).
FROM python:3.14-slim

# --no-cache-dir keeps the image lean; the wheels are cached per layer only.
RUN pip install --no-cache-dir \
      reportlab \
      openpyxl \
      python-docx \
      Pillow \
      matplotlib

# Sanity check at build time so a broken wheel fails the build, not a user run.
RUN python -c "import reportlab, openpyxl, docx, PIL, matplotlib; print('sandbox libs ok')"

# Runs as the unprivileged sandbox user (65534:65534) on a read-only rootfs with
# a host workspace bind-mounted at /work; no writable layer is needed.
CMD ["python", "-"]
