#!/bin/sh
# Both processes live in this container. No Docker API, privileged mode or
# host socket is required. A failed helper stops this container instead of
# silently leaving the UI with an unavailable installer.
set -u
cd /app || exit 1

# Explicit opt-out also keeps custom non-root container deployments usable.
if [ "${GONDAR_INSTALLER_DISABLED:-0}" = "1" ]; then
  echo '[gondar] 已停用容器安装助手，工具体检继续可用' >&2
  exec /app/start.sh "$@"
fi

TOOLS=/opt/gondar-tools
mkdir -p "$TOOLS/bin" "$TOOLS/state" /run/gondar || exit 1
chmod 0755 "$TOOLS" "$TOOLS/bin" /run/gondar || exit 1
chmod 0700 "$TOOLS/state" || exit 1
export PATH="$TOOLS/python/bin:$TOOLS/bin:$PATH"
export GONDAR_HOSTD_SOCKET=/run/gondar/hostd.sock

app=0
helper=0
stopping=0
forward() {
  stopping=1
  [ "$app" -eq 0 ] || kill -TERM "$app" 2>/dev/null || true
  [ "$helper" -eq 0 ] || kill -TERM "$helper" 2>/dev/null || true
}
trap forward INT TERM

/app/gondar-hostd -socket "$GONDAR_HOSTD_SOCKET" -socket-mode 0600 \
  -install-dir "$TOOLS/bin" -state-dir "$TOOLS/state" -python-env "$TOOLS/python" &
helper=$!

# start.sh retains the existing main-program restart/update behavior.
/app/start.sh "$@" &
app=$!

while [ "$stopping" -eq 0 ] && kill -0 "$app" 2>/dev/null && kill -0 "$helper" 2>/dev/null; do
  sleep 1 &
  wait $! || true
done

helper_failed=0
if [ "$stopping" -eq 0 ] && ! kill -0 "$helper" 2>/dev/null; then
  helper_failed=1
  echo '[gondar] 容器安装助手已退出，停止同容器主程序；请检查日志后重启容器' >&2
fi
was_stopping=$stopping
forward

# A trapped signal may interrupt wait before the child has actually exited.
wait "$app" 2>/dev/null
code=$?
if kill -0 "$app" 2>/dev/null; then wait "$app" 2>/dev/null; code=$?; fi
wait "$helper" 2>/dev/null || true
if kill -0 "$helper" 2>/dev/null; then wait "$helper" 2>/dev/null || true; fi
[ "$helper_failed" -eq 0 ] || exit 1
[ "$was_stopping" -eq 0 ] || exit 0
exit "$code"
