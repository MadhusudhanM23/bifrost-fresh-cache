# Runs bin/storebench-linux inside an alpine container on a user-defined Docker
# network shared with the database containers, so the client talks to
# PostgreSQL and Qdrant over the Docker VM's own network instead of through
# Docker Desktop's Windows port proxy (which adds latency that grows with
# message size, see README "Caveats").
#
#   powershell -ExecutionPolicy Bypass -File scripts\run-in-docker.ps1 -Label in-docker -BenchArgs "-dims 1536,384"
#
# The network is created and the containers connected if needed; remove them
# afterwards with scripts\docker-network-cleanup.ps1.
param(
    [string]$Label = "in-docker",
    [string]$BenchArgs = "",
    [string]$Network = "eval-bench-net",
    [string]$PgContainer = "bifrost-pgvector-test",
    [string]$QdrantContainer = "eval-qdrant"
)
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$bifrost = (Resolve-Path (Join-Path $root "..\bifrost")).Path

if (-not (docker network ls --format "{{.Name}}" | Where-Object { $_ -eq $Network })) {
    docker network create $Network | Out-Null
}
foreach ($container in $PgContainer, $QdrantContainer) {
    $networks = docker inspect --format "{{range `$k, `$v := .NetworkSettings.Networks}}{{`$k}} {{end}}" $container
    if (-not ($networks -split " " | Where-Object { $_ -eq $Network })) {
        docker network connect $Network $container
    }
}

# The container has no docker CLI, so image names and engine details are read
# here and passed in to be recorded with the results.
$images = foreach ($container in $PgContainer, $QdrantContainer) {
    "$container=" + (docker inspect --format "{{.Config.Image}} image_id={{.Image}}" $container)
}
$engine = docker info --format "{{.OperatingSystem}}, kernel {{.KernelVersion}}, {{.NCPU}} CPUs, {{.MemTotal}} bytes RAM, server {{.ServerVersion}}"
$notes = "docker=$engine;network=user-defined bridge $Network, client and servers in the same Docker VM"
$extra = @($BenchArgs -split "\s+" | Where-Object { $_ })

docker run --rm --name eval-storebench --network $Network `
    -v "${root}\bin:/app:ro" -v "${root}\results:/results" -v "${bifrost}:/bifrost:ro" `
    alpine:3 /app/storebench-linux -label $Label -out /results -bifrost-dir /bifrost `
    -pg-host $PgContainer -pg-port 5432 -qdrant-host $QdrantContainer -qdrant-port 6334 `
    -containers "$PgContainer,$QdrantContainer" -container-image ($images -join ",") -note $notes @extra
exit $LASTEXITCODE
