# kiseki

kiseki collects a personal lifelog and archives it on your own server. Agents on each machine push browser history and Claude Code sessions to `kiseki serve`, which stores them in SQLite and offers a read-only web interface with full-text search.

- [Design](docs/design.md)
- [API](docs/api.md)
- [Agent](docs/agent.md)
- [Web interface](docs/web.md)
- [Deployment with NixOS and Home Manager](docs/deploy.md)

## Development

```
nix develop
go test ./...
scripts/demo.sh    # web interface with synthetic data at http://127.0.0.1:18090/
```

## License

MIT
