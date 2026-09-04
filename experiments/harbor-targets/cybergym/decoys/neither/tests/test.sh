#!/bin/bash
# UNUSED. environment_mode = "separate" with a pinned docker_image means
# Harbor never injects this file; the real gate is baked at /tests/test.sh
# inside dawn-cybergym-gate:1 (see ../gate/).
echo 0 > /logs/verifier/reward.txt
