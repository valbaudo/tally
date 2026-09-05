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
