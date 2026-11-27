# Cross-build environment for 6.6.0-msm8916 modules (GCC 13, like the stick's kernel).
FROM ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
	gcc-aarch64-linux-gnu binutils-aarch64-linux-gnu make bc flex bison libssl-dev libelf-dev kmod python3 gcc libc6-dev \
	&& rm -rf /var/lib/apt/lists/*
