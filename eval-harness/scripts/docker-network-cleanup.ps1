# Undoes what run-in-docker.ps1 set up: disconnects the database containers
# from the benchmark network and removes it. The containers themselves are left
# running (stop eval-qdrant separately with `docker rm -f eval-qdrant`).
param(
    [string]$Network = "eval-bench-net",
    [string[]]$Containers = @("bifrost-pgvector-test", "eval-qdrant")
)
foreach ($container in $Containers) {
    docker network disconnect $Network $container 2>$null
}
docker network rm $Network
