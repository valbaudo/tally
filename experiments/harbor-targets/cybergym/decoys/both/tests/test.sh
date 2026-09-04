#!/bin/bash
# UNUSED. environment_mode = "separate" with a pinned docker_image means Harbor
# never injects this file; the real gate is baked at /tests/test.sh inside the
# pinned gate image (see ../gate/). It writes no reward on purpose -- if it ever
# runs, that is an infra error, not a verdict.
echo "unused stub -- the real gate is baked into the pinned verifier image" >&2
exit 1
