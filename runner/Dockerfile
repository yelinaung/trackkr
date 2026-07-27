# The base image drops to a non-root user via its own entrypoint;
# root is only used here for build-time package installation.
FROM myoung34/github-runner:ubuntu-jammy@sha256:0f913e32df159864e2269b3daeaf2eef66d34e32f5de170f8e44865f370d31ca

# renovate: datasource=golang-version depName=go versioning=semver
ARG GO_VERSION=1.26.1
# renovate: datasource=go depName=mvdan.cc/gofumpt versioning=semver extractVersion=^v(?<version>.*)$
ARG GOFUMPT_VERSION=0.9.2
# renovate: datasource=go depName=golang.org/x/vuln versioning=semver extractVersion=^v(?<version>.*)$
ARG GOVULNCHECK_VERSION=1.1.4
# renovate: datasource=go depName=github.com/securego/gosec/v2 versioning=semver extractVersion=^v(?<version>.*)$
ARG GOSEC_VERSION=2.24.7
# renovate: datasource=github-releases depName=golangci/golangci-lint versioning=semver extractVersion=^v(?<version>.*)$
ARG GOLANGCI_LINT_VERSION=2.11.3
# renovate: datasource=github-releases depName=goreleaser/goreleaser versioning=semver extractVersion=^v(?<version>.*)$
ARG GORELEASER_VERSION=2.14.3
# renovate: datasource=github-releases depName=gitleaks/gitleaks versioning=semver extractVersion=^v(?<version>.*)$
ARG GITLEAKS_VERSION=8.30.0
# renovate: datasource=pypi depName=semgrep versioning=pep440
ARG SEMGREP_VERSION=1.154.0

# Use bash with pipefail for RUN steps that use curl|tar pipelines.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends jq file && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p "/opt/hostedtoolcache/go/${GO_VERSION}/x64" && \
    curl --proto "=https" --tlsv1.2 -sSf -L "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | \
    tar -xz -C "/opt/hostedtoolcache/go/${GO_VERSION}/x64" --strip-components=1 && \
    touch "/opt/hostedtoolcache/go/${GO_VERSION}/x64.complete"

ENV PATH="/opt/hostedtoolcache/go/${GO_VERSION}/x64/bin:/opt/hostedtoolcache/go/${GO_VERSION}/x64/packages/bin:${PATH}" \
    GOPATH="/opt/hostedtoolcache/go/${GO_VERSION}/x64/packages" \
    GOBIN="/opt/hostedtoolcache/go/${GO_VERSION}/x64/packages/bin"

RUN pip3 install --no-cache-dir semgrep=="${SEMGREP_VERSION}" && \
    go install "mvdan.cc/gofumpt@v${GOFUMPT_VERSION}" && \
    go install "golang.org/x/vuln/cmd/govulncheck@v${GOVULNCHECK_VERSION}" && \
    go install "github.com/securego/gosec/v2/cmd/gosec@v${GOSEC_VERSION}" && \
    curl --proto "=https" --tlsv1.2 -sSf -L "https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh" | \
    sh -s -- -b "${GOBIN}" "v${GOLANGCI_LINT_VERSION}" && \
    curl --proto "=https" --tlsv1.2 -sSf -L "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/gitleaks_${GITLEAKS_VERSION}_linux_x64.tar.gz" | \
    tar -xz -C /usr/local/bin gitleaks && \
    curl --proto "=https" --tlsv1.2 -sSf -L "https://github.com/goreleaser/goreleaser/releases/download/v${GORELEASER_VERSION}/goreleaser_Linux_x86_64.tar.gz" | \
    tar -xz -C /usr/local/bin goreleaser && \
    go clean -modcache
