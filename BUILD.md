# Build notes

There isn't much here on building but let's take a shot at it.

The build script seems to want these env vars:

	-e GITHUB_TOKEN 
	-e DOCKER_USERNAME 
	-e DOCKER_PASSWORD 
	-e DOCKER_REGISTRY 


export DOCKER_USERNAME=yyyyy
export DOCKER_PASSWORD=xxxxx

make goreleaser
