# The image internal/e2e's container leg runs
# (TestTheContainerModeRunsTheSameProtocol). NOTHING BUILDS THIS FILE: the test
# reads its one FROM line and hands that reference to `docker run` unchanged.
# It is a Dockerfile because that is the one manifest Dependabot's `docker`
# ecosystem reads. The pin used to be a Go string, which nothing moves, and it
# was a patch release behind `latest` on the day it moved here.
#
# mirror.gcr.io, NOT Docker Hub. It is Google's mirror of Docker Hub's official
# images: the same bytes under the same digests, served with no token round
# trip. Docker Hub's token service timed out on GitHub's runners for every
# attempt of one CI run, which turned this gate red over a registry rather than
# over the engine. ECR Public's mirror (public.ecr.aws/docker/library) serves
# the same digests too, and was passed over for its anonymous quota: 1 pull a
# second and 500 GB a month per source address, and a runner's address is
# shared with whoever else ran on it this month.
#
# AN EXACT TAG AND ITS DIGEST, never `latest`. A floating tag gives Dependabot
# nothing to move and makes a green run a claim about a build nobody can name
# afterwards. Dependabot moves the tag and the digest together, weekly
# (.github/dependabot.yml), reading the tags from this same mirror, and its
# pull request runs this gate before it merges — so the pin follows `latest`
# without ever being it. The digest is the multi-arch index and is what the
# runtime resolves by; the tag is there for the reader and for Dependabot.
#
# One FROM and nothing else. The test refuses a stage name, a --platform, an
# ARG or a second FROM rather than guess which reference was meant, and
# TestTheContainerLegsImageIsAnExactPinOffDockerHub holds the reference to an
# exact version, a digest and a registry that is not Docker Hub.
FROM mirror.gcr.io/library/alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
