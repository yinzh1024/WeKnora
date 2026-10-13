#!/bin/bash
cd "$(dirname "$0")"
set -a
[ -f .env.lite ] && . ./.env.lite
set +a
export SSRF_WHITELIST_EXTRA=localhost,127.0.0.1,192.168.30.71

# --- DocReader 文档解析服务：随 lite 一起启动 ---
# lite 单二进制只含 gRPC 客户端；PDF/Office 的 builtin 解析需要这个独立 Python 服务。
# 若已在运行则复用，不会重复启动。
DOCREADER_ADDR="${DOCREADER_ADDR:-127.0.0.1:50051}"
DOCREADER_HOST="${DOCREADER_ADDR%%:*}"
DOCREADER_PORT="${DOCREADER_ADDR##*:}"
DOCREADER_PY="./docreader/.venv/bin/python"
DOCREADER_LOG="./data/docreader.log"

port_open() { (exec 3<>"/dev/tcp/${DOCREADER_HOST}/${DOCREADER_PORT}") 2>/dev/null; }

if port_open; then
  echo "[start-lite] docreader 已在 ${DOCREADER_ADDR} 运行，复用"
elif [ -x "$DOCREADER_PY" ]; then
  echo "[start-lite] 启动 docreader (${DOCREADER_ADDR}) ..."
  PYTHONPATH="$PWD" "$DOCREADER_PY" -m docreader.main > "$DOCREADER_LOG" 2>&1 &
  DOCREADER_PID=$!
  for _ in $(seq 1 60); do
    port_open && break
    if ! kill -0 "$DOCREADER_PID" 2>/dev/null; then
      echo "[start-lite] docreader 启动失败，详见 $DOCREADER_LOG"
      break
    fi
    sleep 0.5
  done
  if port_open; then
    echo "[start-lite] docreader 就绪 (pid=$DOCREADER_PID)"
  else
    echo "[start-lite] 警告: docreader 未就绪，详见 $DOCREADER_LOG"
  fi
else
  echo "[start-lite] 警告: 未找到 docreader 虚拟环境 ($DOCREADER_PY)，PDF/Office 解析将失败"
  echo "[start-lite]   修复: cd docreader && uv sync   (然后重跑本脚本)"
fi

exec ./WeKnora-lite
