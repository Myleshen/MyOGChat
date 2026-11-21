# Local MyOGChat code-sandbox Bash image (Phase 2.7).
#
# Built by deploy/sandbox-setup.sh and tagged `myogchat-sandbox-bash` (the
# server's default bash image name), so `run_bash` gets the same developer
# toolbelt as `run_python` instead of the bare stock image.
#
# tools.list is the single source of truth for both sandbox images: add a
# package there, not in a Dockerfile. The list uses Debian package names (the
# Python image is Debian); Alpine names the two that differ, hence the sed.
FROM bash:5.3

COPY tools.list /tmp/tools.list
RUN apk add --no-cache \
      $(sed -e 's/^xz-utils$/xz/' -e 's/^sqlite3$/sqlite/' /tmp/tools.list) \
 && rm -f /tmp/tools.list

# Sanity check at build time so a missing tool fails the build, not a user run.
RUN git --version && rg --version >/dev/null && jq --version >/dev/null && sqlite3 --version >/dev/null

# The runner feeds the script on stdin (`bash -s`).
CMD ["bash", "-s"]
