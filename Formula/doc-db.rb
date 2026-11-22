# NOTE: Comments in this Formula are intentionally written in English,
# following the de facto Homebrew ecosystem convention (homebrew-core and
# nearly all personal taps use English-only). This is an explicit, scoped
# exception to the project-wide "Japanese comments in source code" policy.
#
# Rationale: a Formula is an interface to the Homebrew ecosystem (anyone
# can run `brew edit doc-db`; the file is a candidate for future
# homebrew-core submission; external contributors familiar with Homebrew
# expect English). Keeping it English minimizes friction.
#
# Design references:
#   docs/specs/install/design/DES-002 §3.2 (Formula skeleton)
#   docs/specs/install/requirements/APP-002 (FML-01〜06, VER-01〜09, GUI-01〜05)

class DocDb < Formula
  LEGACY_GO_REVISION = "dcb08abfcaa5491d762b94d4a77369931d5011ad".freeze

  desc "Hybrid search MCP server for Markdown documents"
  homepage "https://github.com/BlueEventHorizon/doc-db-mcp-server"
  # url + tag + revision pin the exact commit (FML-02 / VER-06 / VER-07).
  # tag follows v{version} convention; revision is the commit SHA of that tag.
  # Both values must be updated together at release time and validated by
  # scripts/verify_version_consistency.sh and scripts/verify_release_tag.sh.
  url "https://github.com/BlueEventHorizon/doc-db-mcp-server.git",
      tag:      "v0.3.4",
      revision: "dcb08abfcaa5491d762b94d4a77369931d5011ad"
  license "MIT"

  # macOS 13 (Ventura) minimum (PRE-03).
  depends_on macos: :ventura
  # v0.3.4 still points at the Go source. The next release contains Cargo.toml.
  if stable.specs[:revision] == LEGACY_GO_REVISION
    depends_on "go" => :build
  else
    depends_on "rust" => :build
    depends_on "cmake" => :build
  end

  def fetch
    if active_spec.specs[:revision] == LEGACY_GO_REVISION
      ENV["GOTOOLCHAIN"] = "local"
      system "go", "mod", "download"
    else
      system "cargo", "fetch", *std_cargo_fetch_args
    end
  end

  def install
    if active_spec.specs[:revision] == LEGACY_GO_REVISION
      ENV["GOPROXY"] = "off"
      ENV["GOTOOLCHAIN"] = "local"
      system "go", "build", *std_go_args(ldflags: "-X main.version=#{version}"), "./cmd/docdb"
    else
      system "cargo", "install", *std_cargo_args
    end

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
           if [ ! -e ~/.doc-db/doc-db.yaml ] && [ ! -L ~/.doc-db/doc-db.yaml ]; then
             cp #{share}/doc-db/doc-db.yaml.example ~/.doc-db/doc-db.yaml
           fi
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
    require "json"
    require "net/http"
    require "socket"
    require "timeout"
    require "yaml"

    assert_equal version.to_s, shell_output("#{bin}/doc-db --version").strip

    # Homebrew gives each test its own home directory. Use the installed
    # sample and talk only to the child process on a loopback port.
    sample = YAML.safe_load_file(share/"doc-db/doc-db.yaml.example")
    assert sample.fetch("server").fetch("port").positive?
    # The historical Go server binds all interfaces, so only exercise its CLI.
    next if active_spec.specs[:revision] == LEGACY_GO_REVISION

    port = TCPServer.open("127.0.0.1", 0) { |socket| socket.local_address.ip_port }
    sample.fetch("server")["port"] = port
    sample.fetch("server")["db_path"] = (testpath/"docdb.sqlite").to_s
    sample["log"] = { "path" => "stderr", "level" => "warn" }
    (testpath/".doc-db").mkpath
    (testpath/".doc-db/doc-db.yaml").write(sample.to_yaml)

    log = testpath/"server.log"
    pid = Process.spawn({ "OPENAI_API_DOCDB_KEY" => "homebrew-test-placeholder", "OPENAI_API_KEY" => nil },
                        (bin/"doc-db").to_s, out: log.to_s, err: [:child, :out])
    begin
      Timeout.timeout(10) do
        loop do
          if Process.waitpid(pid, Process::WNOHANG)
            pid = nil
            flunk "MCP server exited during startup:\n#{log.read}"
          end
          TCPSocket.open("127.0.0.1", port, &:close)
          break
        rescue Errno::ECONNREFUSED
          sleep 0.05
        end
      end

      http = Net::HTTP.new("127.0.0.1", port, nil)
      http.open_timeout = 2
      http.read_timeout = 3
      headers = { "Accept" => "application/json, text/event-stream", "Content-Type" => "application/json" }
      initialize_request = {
        jsonrpc: "2.0", id: 1, method: "initialize",
        params:  {
          protocolVersion: "2025-03-26", capabilities: {}, clientInfo: { name: "brew-test", version: "0" },
        },
      }
      response = http.post("/mcp", initialize_request.to_json, headers)
      assert_equal "200", response.code, response.body
      payload = if response["content-type"].to_s.include?("text/event-stream")
        response.body.lines.filter_map { |line| line.delete_prefix("data:").strip if line.start_with?("data:") }
                .find { |data| !data.empty? }
      else
        response.body
      end
      assert payload, "No MCP response payload"
      result = JSON.parse(payload)
      assert_equal 1, result.fetch("id")
      assert_equal "doc-db", result.dig("result", "serverInfo", "name")
      assert_equal version.to_s, result.dig("result", "serverInfo", "version")
    rescue Timeout::Error
      flunk "MCP startup or initialize timed out:\n#{log.read}"
    ensure
      if pid
        begin
          Process.kill("TERM", pid)
        rescue Errno::ESRCH
          # The server already exited.
        end
        begin
          Timeout.timeout(3) { Process.wait(pid) }
        rescue Timeout::Error
          begin
            Process.kill("KILL", pid)
          rescue Errno::ESRCH
            # The server exited during cleanup.
          end
          Process.wait(pid)
        end
      end
    end
  end
end
