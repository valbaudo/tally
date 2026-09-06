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
  # as name@sha256, and reports the same Size dawn's fan sizing reads.
  output = ["type=image,rewrite-timestamp=true,unpack=false"]
}

target "pr-ci-env" {
  inherits = ["reproducible"]
  context  = "pr-ci/environment"
  tags     = ["dawn-pr-ci-env"]
}

target "pr-ci-gate" {
  inherits = ["reproducible"]
  context  = "pr-ci/gate"
  tags     = ["dawn-pr-ci-gate"]
}

group "pr-ci" {
  targets = ["pr-ci-env", "pr-ci-gate"]
}

target "cybergym-env" {
  inherits = ["reproducible"]
  context  = "cybergym/environment"
  tags     = ["dawn-cybergym-env"]
}

target "cybergym-gate" {
  inherits = ["reproducible"]
  context  = "cybergym/gate"
  tags     = ["dawn-cybergym-gate"]
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
  tags     = ["dawn-vdh-adyen-env"]
}

target "vdh-adyen-gate" {
  inherits   = ["reproducible"]
  context    = "vdh-adyen"
  dockerfile = "gate/Dockerfile"
  tags     = ["dawn-vdh-adyen-gate"]
}

group "vdh-adyen" {
  targets = ["vdh-adyen-env", "vdh-adyen-gate"]
}

target "mdash-env" {
  inherits = ["reproducible"]
  context  = "mdash/environment"
  tags     = ["dawn-mdash-env"]
}

target "mdash-gate" {
  inherits = ["reproducible"]
  context  = "mdash/gate"
  tags     = ["dawn-mdash-gate"]
}

target "mdash-env-codex" {
  inherits   = ["reproducible"]
  context    = "mdash/environment"
  dockerfile = "Dockerfile.codex"
  tags       = ["dawn-mdash-env-codex"]
}

group "mdash" {
  targets = ["mdash-env", "mdash-env-codex", "mdash-gate"]
}

target "vdh-hunt-env" {
  inherits = ["reproducible"]
  context  = "vdh-hunt"
  dockerfile = "environment/Dockerfile"
  tags     = ["dawn-vdh-hunt-env"]
}

target "vdh-hunt-gate" {
  inherits   = ["reproducible"]
  context    = "vdh-hunt"
  dockerfile = "gate/Dockerfile.hunt"
  tags       = ["dawn-vdh-hunt-gate"]
}

target "vdh-report-gate" {
  inherits   = ["reproducible"]
  context    = "vdh-hunt"
  dockerfile = "gate/Dockerfile.report"
  tags       = ["dawn-vdh-report-gate"]
}


target "vdh-validate-gate" {
  inherits   = ["reproducible"]
  context    = "vdh-hunt"
  dockerfile = "gate/Dockerfile.validate"
  tags       = ["dawn-vdh-validate-gate"]
}

target "vdh-dedupe-gate" {
  inherits   = ["reproducible"]
  context    = "vdh-hunt"
  dockerfile = "gate/Dockerfile.dedupe"
  tags       = ["dawn-vdh-dedupe-gate"]
}

target "vdh-trace-gate" {
  inherits   = ["reproducible"]
  context    = "vdh-hunt"
  dockerfile = "gate/Dockerfile.trace"
  tags       = ["dawn-vdh-trace-gate"]
}

group "vdh-hunt" {
  targets = ["vdh-hunt-env", "vdh-hunt-gate", "vdh-report-gate",
             "vdh-validate-gate", "vdh-dedupe-gate", "vdh-trace-gate"]
}
