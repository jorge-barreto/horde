package horde

import "embed"

//go:embed docker/Dockerfile docker/entrypoint.sh docker/git-askpass.sh
var WorkerFiles embed.FS
