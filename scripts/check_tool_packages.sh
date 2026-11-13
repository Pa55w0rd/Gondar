#!/usr/bin/env bash
# Offline behavior regression: source only the helper definitions, never deploy.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
eval "$(sed -n '/^tool_package_installed(){/,/^install_tools(){/p' "$root/install.sh" | sed '$d')"
APT_PROXY=() DNF_PROXY=() TOOL_PACKAGE_FAILURES=()
installed=' present '
calls=''
info(){ :; }
warn(){ :; }
tool_package_installed(){ [[ "$installed" == *" $2 "* ]]; }
apt-get(){
  shift 2 # install -y
  calls+="[$*]"
  if [ "$#" -gt 1 ]; then
    installed+=' partial '
    return 100
  fi
  case "$1" in
    absent) return 100 ;;
    *) installed+=" $1 "; return 0 ;;
  esac
}
tool_install_group apt present partial absent valid
[[ "$calls" == '[partial absent valid][absent][valid]' ]] || { printf '逐包补装错误：%s\n' "$calls"; exit 1; }
[[ "${TOOL_PACKAGE_FAILURES[*]}" == 'absent' ]] || { printf '缺失清单错误\n'; exit 1; }
calls=''
tool_install_group apt present partial valid
[[ -z "$calls" ]] || { printf '已装包被重复安装\n'; exit 1; }
rpm(){ [[ "$installed" == *" $2 "* ]]; }
dnf(){ shift 2; calls+="[$*]"; installed+=" $* "; }
tool_install_group dnf present extra
[[ "$calls" == '[extra]' ]] || { printf 'dnf补装错误\n'; exit 1; }
printf '已通过：分组失败、部分成功、逐包补装、失败清单、已装跳过及dnf离线回归\n'
