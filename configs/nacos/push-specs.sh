#!/bin/sh
# push-specs.sh — 把 /specs/*.yaml 推送到 Nacos
#
# 由 docker-compose 的 nacos-init 一次性服务调用：
#   - 镜像：curlimages/curl
#   - bind mount：./configs/nacos → /specs:ro
#   - depends_on：nacos（service_healthy）
#
# Nacos OpenAPI v1 publish config：
#   POST /nacos/v1/cs/configs?dataId=<id>&group=DEFAULT_GROUP&type=yaml
#   body: 表单字段 content=<原始 YAML 文本>
#
# dataId 直接用文件名（extractor-anthropic.yaml 等）—— Flink job 端就是按
# 文件名（含 .yaml 后缀）取配置。

set -eu

NACOS_HOST="${NACOS_HOST:-nacos:8848}"
GROUP="${GROUP:-DEFAULT_GROUP}"
SPECS_DIR="${SPECS_DIR:-/specs}"

echo "[nacos-init] pushing extractor specs to ${NACOS_HOST}, group=${GROUP}"

# 等 Nacos OpenAPI 就绪（健康 endpoint 不一定足够新）
i=0
until curl -sf "http://${NACOS_HOST}/nacos/v1/cs/configs?dataId=__ping__&group=${GROUP}" >/dev/null 2>&1 \
    || [ "$?" = "22" ]; do
    i=$((i+1))
    if [ "$i" -gt 60 ]; then
        echo "[nacos-init] timeout waiting for nacos openapi"
        exit 1
    fi
    sleep 2
done

for f in "${SPECS_DIR}"/extractor-*.yaml; do
    [ -f "$f" ] || continue
    dataId="$(basename "$f")"
    echo "[nacos-init] publish dataId=${dataId}"
    # --data-urlencode 把 YAML 原文当 form field content 提交
    curl -sf -X POST "http://${NACOS_HOST}/nacos/v1/cs/configs" \
        --data-urlencode "dataId=${dataId}" \
        --data-urlencode "group=${GROUP}" \
        --data-urlencode "type=yaml" \
        --data-urlencode "content@${f}" \
        && echo "  ok" \
        || { echo "  FAIL"; exit 1; }
done

echo "[nacos-init] done"
