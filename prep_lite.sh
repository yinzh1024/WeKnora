#!/bin/bash
cd /media/yinzh/ext/WeKnora
if [ ! -f .env.lite ]; then cp .env.lite.example .env.lite; fi
if grep -q '^SYSTEM_AES_KEY=$' .env.lite; then
  sed -i "s|^SYSTEM_AES_KEY=\$|SYSTEM_AES_KEY=$(openssl rand -hex 16)|" .env.lite
fi
if grep -q '^JWT_SECRET=$' .env.lite; then
  sed -i "s|^JWT_SECRET=\$|JWT_SECRET=$(openssl rand -hex 16)|" .env.lite
fi
mkdir -p data/files
chmod +x start-lite.sh build_lite.sh
rm -f feat-sibling-image-refs.patch
nohup bash build_lite.sh > data/build.log 2>&1 < /dev/null &
echo "build_started pid=$!"
