# Every image a protocol pins is built from here and nowhere else: the digest
# in the protocol source is only a fact if the build behind it is a pure
# function of this tree. Measured 2026-09-05 (OrbStack, docker 29.4.0, buildx
# v0.33.0, arm64): a plain `docker build` of byte-identical layers yields a
# different digest every time, for three reasons, each removed by one line.
target "reproducible" {
  # Config `created`, history timestamps and (via rewrite-timestamp) every
  # file mtime are stamped with this epoch. A build arg, not an env var, so
  # the digest cannot depend on the invoking shell.
  args   = { SOURCE_DATE_EPOCH = "0" }
  # buildx attaches a provenance attestation that varies per build and rides
  # as a second manifest in the index, moving RepoDigests[0] with every layer
  # identical. SBOM likewise.
  attest = ["type=provenance,disabled=true", "type=sbom,disabled=true"]
  # COPY/RUN layers otherwise carry build-time directory mtimes. OrbStack's
  # containerd exporter refuses rewrite-timestamp together with its default
  # unpack, so the image is unpacked on first run instead; it runs, resolves
  # as name@sha256, and reports the same Size tally's fan sizing reads.
  output = ["type=image,rewrite-timestamp=true,unpack=false"]
}

# Every image the protocols pin, so `docker buildx bake` with no argument
# builds the whole set — which is what repin at the repo root relies on.
group "default" {
  targets = ["pr-ci", "cybergym", "vdh-adyen", "mdash", "vdh-hunt"]
}

target "pr-ci-env" {
  inherits = ["reproducible"]
  context  = "pr-ci/environment"
  tags     = ["tally-pr-ci-env"]
}

target "pr-ci-gate" {
  inherits = ["reproducible"]
  context  = "pr-ci/gate"
  tags     = ["tally-pr-ci-gate"]
}

group "pr-ci" {
  targets = ["pr-ci-env", "pr-ci-gate"]
}

target "cybergym-env" {
  inherits = ["reproducible"]
  context  = "cybergym/environment"
  tags     = ["tally-cybergym-env"]
}

target "cybergym-gate" {
  inherits = ["reproducible"]
  context  = "cybergym/gate"
  tags     = ["tally-cybergym-gate"]
}

group "cybergym" {
  targets = ["cybergym-env", "cybergym-gate"]
}

# One vendored copy of adyen-shopware6 (MIT), shared by both images: the gate
# checks citations against source the agent never touched, and building both
# from one tree is what guarantees they start identical.
target "vdh-adyen-env" {
  inherits   = ["reproducible"]
  context    = "vdh-adyen"
  dockerfile = "environment/Dockerfile"
  tags     = ["tally-vdh-adyen-env"]
}

target "vdh-adyen-gate" {
  inherits   = ["reproducible"]
  context    = "vdh-adyen"
  dockerfile = "gate/Dockerfile"
  tags     = ["tally-vdh-adyen-gate"]
}

group "vdh-adyen" {
  targets = ["vdh-adyen-env", "vdh-adyen-gate"]
}

target "mdash-env" {
  inherits = ["reproducible"]
  context  = "mdash/environment"
  tags     = ["tally-mdash-env"]
}

target "mdash-gate" {
  inherits = ["reproducible"]
  context  = "mdash/gate"
  tags     = ["tally-mdash-gate"]
}

target "mdash-env-codex" {
  inherits   = ["reproducible"]
  context    = "mdash/environment"
  dockerfile = "Dockerfile.codex"
  tags       = ["tally-mdash-env-codex"]
}

group "mdash" {
  targets = ["mdash-env", "mdash-env-codex", "mdash-gate"]
}

target "vdh-hunt-env" {
  inherits = ["reproducible"]
  context  = "vdh-hunt"
  dockerfile = "environment/Dockerfile"
  tags     = ["tally-vdh-hunt-env"]
}

target "vdh-hunt-env-codex" {
  inherits   = ["reproducible"]
  context    = "vdh-hunt"
  dockerfile = "environment/Dockerfile.codex"
  tags       = ["tally-vdh-hunt-env-codex"]
}

target "vdh-hunt-gate" {
  inherits   = ["reproducible"]
  context    = "vdh-hunt"
  dockerfile = "gate/Dockerfile.hunt"
  tags       = ["tally-vdh-hunt-gate"]
}

target "vdh-report-gate" {
  inherits   = ["reproducible"]
  context    = "vdh-hunt"
  dockerfile = "gate/Dockerfile.report"
  tags       = ["tally-vdh-report-gate"]
}


target "vdh-validate-gate" {
  inherits   = ["reproducible"]
  context    = "vdh-hunt"
  dockerfile = "gate/Dockerfile.validate"
  tags       = ["tally-vdh-validate-gate"]
}

target "vdh-dedupe-gate" {
  inherits   = ["reproducible"]
  context    = "vdh-hunt"
  dockerfile = "gate/Dockerfile.dedupe"
  tags       = ["tally-vdh-dedupe-gate"]
}

target "vdh-trace-gate" {
  inherits   = ["reproducible"]
  context    = "vdh-hunt"
  dockerfile = "gate/Dockerfile.trace"
  tags       = ["tally-vdh-trace-gate"]
}

group "vdh-hunt" {
  targets = ["vdh-hunt-env", "vdh-hunt-env-codex", "vdh-hunt-gate",
             "vdh-report-gate", "vdh-validate-gate", "vdh-dedupe-gate",
             "vdh-trace-gate"]
}
