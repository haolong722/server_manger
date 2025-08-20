#!/bin/bash

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m' # No Color

# 日志函数
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}
log_error() {
    echo -e "${RED}[ERROR]${NC} $1" >&2
}
log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

# 检查命令是否存在
check_command() {
    command -v "$1" >/dev/null 2>&1
}

# 检查是否以 root 运行
if [ "$(id -u)" != "0" ]; then
    log_error "此脚本需要 root 权限运行，请使用 sudo 或以 root 用户执行"
    exit 1
fi

# 项目信息
VERSION="0.1.0"
REPO_URL="https://github.com/haolong722/server_manger"
RAW_URL="https://raw.githubusercontent.com/haolong722/server_manger/main"
INSTALL_DIR="/usr/local/server-manager"
CONFIG_FILE="$INSTALL_DIR/config.toml"
SERVICE_NAME="server-manager"
SERVICE_USER="servermgr"

# 创建专用用户
create_service_user() {
    log_info "创建专用用户 $SERVICE_USER..."
    if ! id "$SERVICE_USER" >/dev/null 2>&1; then
        useradd -r -s /bin/false "$SERVICE_USER"
        if [ $? -ne 0 ]; then
            log_error "创建用户 $SERVICE_USER 失败"
            exit 1
        fi
    fi
}

# 检查依赖
check_dependencies() {
    log_info "检查依赖..."

    # 检查 curl
    if ! check_command curl; then
        log_info "安装 curl..."
        if check_command apt-get; then
            apt-get update && apt-get install -y curl
        elif check_command yum; then
            yum install -y curl
        else
            log_error "不支持的包管理器，请手动安装 curl"
            exit 1
        fi
        if [ $? -ne 0 ]; then
            log_error "安装 curl 失败"
            exit 1
        fi
    fi

    # 检查 Go
    if ! check_command go; then
        log_info "安装 Go 1.21.0..."
        curl -Ls "https://golang.org/dl/go1.21.0.linux-amd64.tar.gz" -o /tmp/go.tar.gz
        tar -C /usr/local -xzf /tmp/go.tar.gz
        echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile
        export PATH=$PATH:/usr/local/go/bin
        rm /tmp/go.tar.gz
        if ! check_command go; then
            log_error "安装 Go 失败"
            exit 1
        fi
    fi

    # 检查 MySQL
    if ! check_command mysql; then
        log_info "安装 MySQL..."
        if check_command apt-get; then
            apt-get update && apt-get install -y mysql-server
        elif check_command yum; then
            yum install -y mariadb-server
            systemctl enable mariadb
            systemctl start mariadb
        else
            log_error "不支持的包管理器，请手动安装 MySQL"
            exit 1
        fi
        if [ $? -ne 0 ]; then
            log_error "安装 MySQL 失败"
            exit 1
        fi
    fi

    # 检查 ufw（防火墙）
    if check_command ufw; then
        log_info "配置防火墙，开放 8080 端口..."
        ufw allow 8080
        ufw --force enable
    else
        log_warning "未检测到 ufw，建议手动配置防火墙开放 8080 端口"
    fi
}

# 获取用户输入
get_user_input() {
    log_info "请提供配置信息："
    read -p "请输入 MySQL 用户名（默认 root）： " DB_USER
    DB_USER=${DB_USER:-root}
    read -sp "请输入 MySQL 密码： " DB_PASS
    echo
    read -p "请输入 MySQL 主机（默认 localhost）： " DB_HOST
    DB_HOST=${DB_HOST:-localhost}
    read -p "请输入 MySQL 端口（默认 3306）： " DB_PORT
    DB_PORT=${DB_PORT:-3306}
    read -p "请输入数据库名称（默认 vpn）： " DB_NAME
    DB_NAME=${DB_NAME:-vpn}
    read -p "请输入管理面板用户名（默认 admin）： " AUTH_USER
    AUTH_USER=${AUTH_USER:-admin}
    read -sp "请输入管理面板密码： " AUTH_PASS
    echo
    read -p "请输入最小端口（默认 1000）： " MIN_PORT
    MIN_PORT=${MIN_PORT:-1000}
    read -p "请输入最大端口（默认 65535）： " MAX_PORT
    MAX_PORT=${MAX_PORT:-65535}
    read -p "请输入监听地址（默认 0.0.0.0:8080）： " BIND_ADDR
    BIND_ADDR=${BIND_ADDR:-0.0.0.0:8080}
}

# 创建配置文件
create_config() {
    log_info "创建配置文件 $CONFIG_FILE..."
    mkdir -p "$INSTALL_DIR"
    cat > "$CONFIG_FILE" <<EOF
[database]
user = "$DB_USER"
password = "$DB_PASS"
host = "$DB_HOST"
port = "$DB_PORT"
name = "$DB_NAME"

[auth]
username = "$AUTH_USER"
password = "$AUTH_PASS"

[port]
min = $MIN_PORT
max = $MAX_PORT

[server]
addr = "$BIND_ADDR"
EOF
    if [ $? -ne 0 ]; then
        log_error "创建配置文件失败"
        exit 1
    fi
    chmod 600 "$CONFIG_FILE"
    chown "$SERVICE_USER:$SERVICE_USER" "$CONFIG_FILE"
}

# 初始化数据库
init_database() {
    log_info "初始化数据库 $DB_NAME..."
    mysql -u"$DB_USER" -p"$DB_PASS" -h"$DB_HOST" -P"$DB_PORT" -e "CREATE DATABASE IF NOT EXISTS $DB_NAME;"
    if [ $? -ne 0 ]; then
        log_error "创建数据库 $DB_NAME 失败，请检查 MySQL 凭据"
        exit 1
    fi
}

# 下载并安装项目
install_project() {
    log_info "下载项目文件..."
    mkdir -p "$INSTALL_DIR/templates"
    curl -Ls "$RAW_URL/main.go" -o "$INSTALL_DIR/main.go"
    if [ $? -ne 0 ]; then
        log_error "下载 main.go 失败，请检查 $RAW_URL/main.go"
        exit 1
    fi
    curl -Ls "$RAW_URL/templates/servers.html" -o "$INSTALL_DIR/templates/servers.html"
    if [ $? -ne 0 ]; then
        log_error "下载 servers.html 失败，请检查 $RAW_URL/templates/servers.html"
        exit 1
    fi
    curl -Ls "$RAW_URL/templates/login.html" -o "$INSTALL_DIR/templates/login.html"
    if [ $? -ne 0 ]; then
        log_error "下载 login.html 失败，请检查 $RAW_URL/templates/login.html"
        exit 1
    fi

    # 静态资源通过 CDN 加载
    log_info "静态资源（Bootstrap、jQuery）将通过 CDN 加载，无需本地下载"

    # 编译 Go 程序
    log_info "编译项目..."
    cd "$INSTALL_DIR"
    go mod init server-manager
    go mod tidy
    go build -o server-manager main.go
    if [ $? -ne 0 ]; then
        log_error "编译失败，请检查 Go 环境和代码"
        exit 1
    fi
    chown -R "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR"
    chmod +x "$INSTALL_DIR/server-manager"
}

# 配置 systemd 服务
setup_systemd() {
    log_info "配置 systemd 服务..."
    cat > /etc/systemd/system/$SERVICE_NAME.service <<EOF
[Unit]
Description=Server Manager Service
After=network.target mysql.service

[Service]
ExecStart=$INSTALL_DIR/server-manager
WorkingDirectory=$INSTALL_DIR
Restart=always
User=$SERVICE_USER
Group=$SERVICE_USER

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME"
    systemctl start "$SERVICE_NAME"
    if [ $? -ne 0 ]; then
        log_error "启动 systemd 服务失败"
        exit 1
    fi
}

# 主函数
main() {
    log_info "开始安装 Server Manager v$VERSION..."
    check_dependencies
    create_service_user
    get_user_input
    create_config
    init_database
    install_project
    setup_systemd
    log_info "安装完成！Server Manager 正在运行，访问 http://<your-server-ip>:8080"
    log_info "管理面板用户名：$AUTH_USER"
    log_warning "请检查 $CONFIG_FILE 确保配置正确"
    log_warning "建议手动验证 $RAW_URL/install.sh 的内容以确保安全"
    log_warning "已开放 8080 端口，建议配置 HTTPS（参考：https://certbot.eff.org）"
}

main