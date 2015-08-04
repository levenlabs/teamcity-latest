# teamcity-latest

A simple REST api for accessing artifacts from the latest builds of teamcity
projects. It will transparently use the teamcity rest api to find and retrieve
the artifacts.

## Usage

Parameters can be specified through command line (`--rest-user`), environment
variable (`TEAMCITY_LATEST_REST_USER`), or configuration file (see `--example`).
Both `--rest-user` and `--rest-password` (or their environment variable/config
file equivalents) are required.

A request to teamcity-latest looks like this:

    GET http://localhost:8112/buildTypeID/tag/artifactName

And the response's body will be that artifact, or an error
