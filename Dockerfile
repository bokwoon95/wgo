# docker build --tag wgotest .
# docker run --interactive --tty --volume "$(pwd)":/wgo wgotest

FROM golang:latest

RUN apt-get update && apt-get install --yes bash

WORKDIR /wgo

CMD ["/bin/bash"]
