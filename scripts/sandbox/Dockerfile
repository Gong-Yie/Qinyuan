# 钦原执行沙箱镜像（M17，docs/24）
#
# 只装"做实验要用"的工具：python3 / JDK / 网络工具 / 解包。
# 不装 docker、不装 sudo、不加 docker 组 —— 容器里跑的是不可信命令。
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive \
    HOME=/home/sandbox \
    LANG=C.UTF-8

RUN apt-get update && apt-get install -y --no-install-recommends \
        python3 python3-pip python3-venv \
        openjdk-17-jdk-headless \
        curl wget netcat-openbsd dnsutils \
        unzip zip jq file binutils \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /job /home/sandbox && chown -R 1000:1000 /job /home/sandbox

USER 1000:1000
WORKDIR /job

# 默认不跑守护进程：由 docker run 传入 `timeout … bash -s`。
CMD ["bash"]
