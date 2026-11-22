#!/bin/sh

# Only emit whitelisted locale tags to avoid config.js injection from env values.
RUNTIME_DEFAULT_LOCALE=""
case "${DEFAULT_LOCALE:-}" in
  zh-CN|zh-TW|en-US|ru-RU|ko-KR|ja-JP) RUNTIME_DEFAULT_LOCALE="${DEFAULT_LOCALE}" ;;
esac

# 生成运行时配置文件，注入环境变量到前端
FILE_MB=${MAX_FILE_SIZE_MB:-50}
SKILL_MB=${MAX_SKILL_BUNDLE_SIZE_MB:-256}
# FAQ 单次导入条数上限，与后端 WEKNORA_FAQ_IMPORT_MAX_ENTRIES 对齐。
#
# 支持的输入范围：可选的 "+"、十进制数字、首尾的 ASCII 空白（空格/Tab/CR/LF），
# 允许前导零。这是管理员在部署配置里实际会写的形状。
#
# 为什么必须规范化：config.js 是普通脚本，"01000" 在那里是八进制 512；
# " 2000 " / "+2000" 在 shell 里被拒会回落 1000。两边一旦不一致，前端会拦住
# 后端本来允许的请求。
#
# 已知且刻意不处理：Unicode 空白（如 U+00A0、垂直制表符）在 shell 的 [[:space:]]
# 里不算空白，后端 TrimSpace 却会去掉，结果前端回落 1000、后端按实际数字生效。
# 这类输入只可能来自手写配置错误，误差在 1000 量级，不会放大后端的条数上限，
# 因此不为它引入完整的 unicode.IsSpace 复刻。中间夹空白的 "20 00" 两边都已
# 回落 1000，不再有差异。
trim_ws() {
  # Only trim the ends: stripping inner whitespace is what turns a typo like
  # "20 00" into a value the server will not accept.
  printf '%s' "$1" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'
}
FAQ_ENTRIES=$(trim_ws "${WEKNORA_FAQ_IMPORT_MAX_ENTRIES:-}")
FAQ_ENTRIES=${FAQ_ENTRIES#+}
case "$FAQ_ENTRIES" in
  ''|*[!0-9]*) FAQ_ENTRIES=1000 ;;
esac
# 去掉前导零（"01000" -> "1000"），避免被 JS 当成八进制。
FAQ_ENTRIES=$(printf '%s' "$FAQ_ENTRIES" | sed 's/^0*//')
[ -n "$FAQ_ENTRIES" ] || FAQ_ENTRIES=0
[ "$FAQ_ENTRIES" -gt 0 ] 2>/dev/null || FAQ_ENTRIES=1000
if [ "$SKILL_MB" -lt "$FILE_MB" ] 2>/dev/null; then
  SKILL_MB=$FILE_MB
fi
if [ "$SKILL_MB" -gt 512 ] 2>/dev/null; then
  SKILL_MB=512
fi

cat > /usr/share/nginx/html/config.js << EOF
window.__RUNTIME_CONFIG__ = {
  MAX_FILE_SIZE_MB: ${FILE_MB},
  MAX_SKILL_BUNDLE_SIZE_MB: ${SKILL_MB},
  FAQ_IMPORT_MAX_ENTRIES: ${FAQ_ENTRIES},
  DEFAULT_LOCALE: "${RUNTIME_DEFAULT_LOCALE}"
};
EOF

# 处理 nginx 配置。
# 两个上限分开注入：全站保持知识库的 MAX_FILE_SIZE，只有技能 zip 上传的两条
# 集合路由放宽到 MAX_SKILL_BUNDLE_SIZE（不含 /install、PATCH 等子路径）。
# 合成一个全站上限会让每个上传端点都能收到技能包那么大的 body。
# client_max_body_size 计的是整个请求体（文件 + multipart 分隔符/文件名等），
# 前端按文件本身大小校验。上限若恰等于 FILE_MB，正好 FILE_MB 的文件会被
# nginx 以 413 拒绝，所以给 multipart 开销留 1M 余量。
export MAX_FILE_SIZE=$((FILE_MB + 1))M
export MAX_SKILL_BUNDLE_SIZE=${SKILL_MB}M
export APP_HOST=${APP_HOST:-app}
export APP_PORT=${APP_PORT:-8080}
export APP_SCHEME=${APP_SCHEME:-http}
envsubst '${MAX_FILE_SIZE} ${MAX_SKILL_BUNDLE_SIZE} ${APP_HOST} ${APP_PORT} ${APP_SCHEME}' \
  < /etc/nginx/templates/default.conf.template > /etc/nginx/conf.d/default.conf

# 启动 nginx
exec nginx -g 'daemon off;'
