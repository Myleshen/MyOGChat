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
FROM python:3.15-slim

# The shared CLI toolbelt (git, curl, ripgrep, jq, sqlite3, poppler, ImageMagick,
# make, archives…). tools.list is the single source of truth for both sandbox
# images — Dockerfile.bash installs the same list.
COPY tools.list /tmp/tools.list
RUN apt-get update \
 && xargs -r -a /tmp/tools.list apt-get install -y --no-install-recommends \
 && rm -rf /var/lib/apt/lists/* /tmp/tools.list

# --no-cache-dir keeps the image lean; the wheels are cached per layer only.
RUN pip install --no-cache-dir \
      reportlab \
      openpyxl \
      python-docx \
      Pillow \
      matplotlib

# Sanity check at build time so a broken wheel fails the build, not a user run.
RUN python -c "import reportlab, openpyxl, docx, PIL, matplotlib; print('sandbox libs ok')"

# Never run as root: the server passes --user <service uid>:<gid> (or nobody
# when it runs as root) on every `docker run`; a read-only rootfs needs no
# writable layer and a host workspace is bind-mounted at /work.
CMD ["python", "-"]
