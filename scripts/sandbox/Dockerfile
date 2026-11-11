# 钦原执行沙箱镜像（M17/M18，docs/24）
#
# 关键点：**JDK 8**。目标题（jetty:9.4.49-jdk8）跑在 JDK8 上，
# 用 JDK17 造序列化载荷会因为 svuid / 字段布局对不上而失败（run #43 实测）。
# 这里用 Temurin 8 打底，保证 javac/java 与目标同代。
#
# 不装 docker、不装 sudo、不加 docker 组 —— 容器里跑的是不可信命令。
FROM eclipse-temurin:8-jdk

ENV DEBIAN_FRONTEND=noninteractive \
    HOME=/home/sandbox \
    LANG=C.UTF-8

RUN apt-get update && apt-get install -y --no-install-recommends \
        python3 python3-pip \
        curl wget netcat-openbsd dnsutils \
        unzip zip jq file binutils \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /job /home/sandbox && chown -R 1000:1000 /job /home/sandbox

USER 1000:1000
WORKDIR /job

# 默认不跑守护进程：由 docker run 传入 `timeout … bash -s`。
CMD ["bash"]
