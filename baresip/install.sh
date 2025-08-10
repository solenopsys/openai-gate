#!/usr/bin/env bash
#
# build_baresip_opus.sh — сборка baresip с Opus на Fedora 41+
#
#  • устанавливает зависимости
#  • клонирует re, rem, baresip в ~/src
#  • собирает через CMake и ставит в /usr
#
set -euo pipefail

SRC="$HOME/src"             # куда клонировать исходники
JOBS=$(nproc)               # параллелизм make
PREFIX="/usr"               # ставим прямо в /usr -> не нужен ld.so.conf.d

echo "==> Installing build dependencies..."
sudo dnf -y install git gcc make cmake \
    opus opus-devel \
    alsa-lib-devel openssl-devel libcurl-devel libuuid-devel \
    libv4l-devel libsrtp-devel speexdsp-devel \
    pulseaudio-libs-devel pipewire-devel libtool

# если раньше ставили в /usr/local, подчистим старые библиотеки
sudo rm -f /usr/local/lib*/lib{re,rem}.so* \
            /usr/local/lib*/baresip/modules/*.so 2>/dev/null || true

mkdir -p "$SRC"
cd "$SRC"

clone() {
  local url="$1"
  local dir="${url##*/}"    # например, re.git
  dir="${dir%.git}"         # → re
  if [[ -d "$dir/.git" ]]; then
    echo "==> Updating $dir ..."
    git -C "$dir" pull --ff-only
  else
    echo "==> Cloning $dir ..."
    git clone --depth 1 "$url" "$dir"
  fi
}

build() {
  local dir="$1"
  echo "==> Building $dir ..."
  cd "$SRC/$dir"
  mkdir -p build && cd build
  cmake .. -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="$PREFIX"
  make -j"$JOBS"
  echo "==> Installing $dir ..."
  sudo make install
}

echo "==> Fetching sources..."
clone https://github.com/baresip/re.git
clone https://github.com/baresip/rem.git
clone https://github.com/baresip/baresip.git

build re
build rem
build baresip   # Opus обнаружится автоматически благодаря opus-devel

sudo ldconfig    # обновляем кэш библиотек на всякий случай

echo -e "\n✅  Baresip установлен."
echo "   Проверяем поддержку Opus:"
echo "   baresip -v | grep -i '^aucodec.*opus' || echo '❌ Opus не найден!'"
