# NOTE: Comments in this Formula are intentionally written in English,
# following the de facto Homebrew ecosystem convention (homebrew-core and
# nearly all personal taps use English-only). This is an explicit, scoped
# exception to the project-wide "Japanese comments in source code" policy.
#
# Rationale: a Formula is an interface to the Homebrew ecosystem (anyone
# can run `brew edit doc-db`; the file is a candidate for future
# homebrew-core submission; external contributors familiar with Homebrew
# expect English). Keeping it English minimizes friction. Go source code
# under cmd/ and internal/ continues to follow the project policy and is
# written in Japanese.
#
# Design references:
#   docs/specs/install/design/DES-002 §3.2 (Formula skeleton)
#   docs/specs/install/requirements/APP-002 (FML-01〜06, VER-01〜09, GUI-01〜05)

class DocDb < Formula
  desc "Hybrid search MCP server for Markdown documents"
  homepage "https://github.com/BlueEventHorizon/doc-db-mcp-server"
  # url + tag + revision pin the exact commit (FML-02 / VER-06 / VER-07).
  # tag follows v{version} convention; revision is the commit SHA of that tag.
  # Both values must be updated together at release time and validated by
  # scripts/verify_version_consistency.sh and scripts/verify_release_tag.sh.
  url "https://github.com/BlueEventHorizon/doc-db-mcp-server.git",
      tag:      "v0.1.7",
      revision: "0000000000000000000000000000000000000000"
  license "MIT"

  # macOS 13 (Ventura) minimum (PRE-03).
  depends_on macos: :ventura
  # Go toolchain required only at build time (PRE-04 / FML-03).
  depends_on "go" => :build

  def install
    # ldflags injects the canonical version (VER-02). `version.to_s` is the
    # tag value with the leading "v" stripped, e.g. tag "v0.1.0" → "0.1.0".
    # That matches the contents of the VERSION file in the repo root.
    system "go", "build",
           "-trimpath",
           "-ldflags", "-s -w -X main.version=#{version}",
           "-o", bin/"doc-db",
           "./cmd/docdb"

    # Ship the config sample so caveats can `cp` it to ~/.doc-db (GUI-04).
    (share/"doc-db").install "doc-db.yaml.example"
  end

  def caveats
    <<~EOS
      doc-db is installed as `doc-db` and is on your PATH.
      doc-db speaks MCP over Streamable HTTP (DES-001 §1). You must start
      the server before any MCP client can connect to it.

      1) Prepare the configuration file (required, fail-fast if missing):
           mkdir -p ~/.doc-db
           cp #{share}/doc-db/doc-db.yaml.example ~/.doc-db/doc-db.yaml
           # Edit ~/.doc-db/doc-db.yaml (port, db_path, embedding model, etc.).

      2) Export your OpenAI API key (secrets are env-only, never in YAML):
           export OPENAI_API_DOCDB_KEY=sk-...

      3) Start the server (foreground; use launchd or similar for persistence):
           doc-db
           # Listens on the port specified in doc-db.yaml (default 58080).

      4) Register with Claude Code (Streamable HTTP transport):
           # User scope (all of your projects):
           claude mcp add --transport http -s user doc-db http://localhost:58080/mcp
           # Local scope (this project only):
           claude mcp add --transport http doc-db http://localhost:58080/mcp

      5) Register with Claude Desktop. Add to
         ~/Library/Application Support/Claude/claude_desktop_config.json:

           {
             "mcpServers": {
               "doc-db": {
                 "url": "http://localhost:58080/mcp"
               }
             }
           }

         Then restart the Claude app.
         (If you change `server.port` in doc-db.yaml, update the URL here too.)

      Full documentation: #{homepage}
    EOS
  end

  test do
    # smoke test: --version must exit immediately with the version string,
    # without loading the config file or requiring an API key (VER-03).
    output = shell_output("#{bin}/doc-db --version")
    assert_match version.to_s, output
  end
end
