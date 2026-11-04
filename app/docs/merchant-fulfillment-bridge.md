# 店小助发货桥接

这个接口让独立运行的店小助读取本商城的已付款订单，把卡密、资料链接或物流单号写回商城订单。买家仍在原商城的订单详情中查看交付内容；不要求买家把邮箱、订单密码或付款截图发到客服聊天里。

这是代码接入能力，尚未进行真实店铺发货验证。接口连接测试只检查配置和协议，不发货。

## 哪些订单可以用

- 仅处理主站的 **manual（人工交付）** 商品；商城原有 **auto（自动发卡）** 商品继续由商城分配库存，桥接不会重复发卡。
- 一条桥接订单对应一个只有一项商品的叶子订单，订单号使用其 `order_no`。商城正常拆出的 manual 子订单可以使用；父订单、旧的多项叶子订单、分销商订单暂不开放。
- 必须有已付款记录，实付金额一致，而且整个父子订单中没有退款记录、部分退款或取消状态。数据不一致时停止交付，交给商家核实。
- `card` 写入已经由店小助分配的卡密；`link` 写入商家提供的资料链接及说明，不上传或托管文件；`physical` 登记商家已经取得的物流公司与运单号，不向快递公司下单，也不代表包裹已经揽收。
- 交付写入现有 `fulfillments` 表并更新订单状态。买家可在商城订单详情、交付文本下载中查看；本接口不额外发送邮件、Telegram 或平台聊天消息。

## 启用

在商城后端进程设置两个独立环境变量，然后重启后端：

| 环境变量 | 内容 |
| --- | --- |
| `MERCHANT_FULFILLMENT_SHOP_ID` | 店铺标识，1—64 个英文字母、数字、短横线或下划线，例如 `my-shop` |
| `MERCHANT_FULFILLMENT_SECRET` | 单独生成的随机密钥，至少 32 字节；不要复用管理员密码、支付密钥或平台授权令牌 |

可用 `python3 -c 'import secrets; print(secrets.token_urlsafe(32))'` 在自己的机器上生成密钥。仅保存到私有进程配置，切勿提交仓库或粘贴到公开截图。

两个变量都不设置时，桥接路由不开放。只配置一个、密钥太短或店铺标识不合法时，后端拒绝启动。远程连接使用 HTTPS，反向代理必须保留原始路径和查询参数，不能记录请求正文或认证请求头。

在店小助的“订单与发货”中新增独角商城连接，填写：

1. 接口基础地址：`https://自己的商城域名/api/v1/merchant-fulfillment`。
2. 与商城一致的店铺标识、独立密钥。
3. 点击“保存订单来源”，再测试连接。先保持自动处理关闭。
4. 按返回的商品 ID、SKU ID 配置发货规则和最低实付金额，再用测试订单试运行。
5. 确认订单、数量和交付内容后，才进行实际处理，并在买家订单页核对结果。

如需演练，使用独立测试商城、测试库存和测试订单，不对真实买家尝试发货。

## 接口与签名

所有接口，包括健康检查，都必须签名。协议标识为 `uecb-fulfillment-v1`，平台标识为 `dujiao`。

签名输入是以下五项，用换行符连接，结尾不额外加换行：

```text
HTTP_METHOD
原始路径和查询字符串
Unix秒时间戳
32位十六进制随机nonce
原始请求正文的SHA256十六进制摘要
```

使用独立密钥计算上述字符串的 HMAC-SHA256，结果编码为十六进制；请求头为 `X-Fulfillment-Timestamp`、`X-Fulfillment-Nonce`、`X-Fulfillment-Signature`。GET 正文为空字节。时间误差最多 300 秒，签名恒时比较；有效时间窗内的 nonce 不可重复。重试必须重新生成 nonce 和签名，但保留原业务幂等键及交付正文。

| 方法和路径 | 功能 |
| --- | --- |
| `GET /health` | 返回店铺身份和 `card`、`link`、`physical` 能力，不改变订单 |
| `GET /orders?limit=50&cursor=...` | 按内部 ID 顺序分页返回符合条件的订单号；`next_cursor` 为 `null` 表示本轮结束 |
| `GET /orders/{order_no}` | 返回付款、退款、商品、数量及实付金额，不返回邮箱、密码、卡密或地址 |
| `POST /deliveries` | 原子写入单个 manual 叶子订单的交付结果 |

每轮扫描从空 cursor 开始走完所有页面，确保后付款的旧订单不漏掉。重复订单由持久化的交付幂等处理。

订单身份为 `user:<用户ID>`，游客为 `guest:<父订单ID>`；这些是商城作用域的身份，不能当成闲鱼或淘宝聊天账号。单行 `amount` 使用叶子订单优惠后的真实实付，绝不使用商品标价。`shipping_amount` 为 `0.00`，该接口当前不拆出单独运费金额。

`POST /deliveries` 的正文包含 `idempotency_key`、`order_id`、`line_id`、`buyer_id`、`item_id`、`sku_id`、`quantity`、`amount`、`currency`、`payload`，与店小助 `BridgeAdapter` 一致。`payload` 只接受：

```json
{"type":"card","cards":["仅演示，不是真实卡密"]}
{"type":"link","content":"商家自己的资料链接及提取说明"}
{"type":"physical","carrier":"物流公司","tracking_number":"已有运单号"}
```

卡密数量必须等于购买数量，最多 10,000 条，每条最多 10,000 个 Unicode 字符且不能含控制字符；同时单次请求最多 256 KB，较大的批次应先由商家核对。响应直接返回 JSON，不套 `data` 层。

## 重复请求与异常

交付事务按父订单、子订单顺序加锁，并在锁内重新核对付款、退款、商品、买家、数量和金额。交付记录的 `order_id` 唯一；相同订单和幂等键、相同正文再次提交，返回原 `receipt`。不同幂等键或修改正文不能再次交付。

- 成功：HTTP 200，`{"status":"sent","receipt":"稳定标识"}`。不返回交付正文。
- 确定未写入的业务拒绝：HTTP 200，`{"status":"failed",...}`。
- 数据库或提交异常：HTTP 500，`{"status":"unknown",...}`，不暴露内部错误。
- 验签失败、超限、格式错误：HTTP 错误，不执行交付。

超时并不代表没有发出。需要核实时，可用新的 nonce 重签同一份交付请求，保留原 `idempotency_key` 和原卡密；已完成的写入会返回原结果。不得换新卡密或新键再次发货。服务重启后短期 nonce 缓存会重建，业务幂等记录仍由数据库保存。

## 开发验证

使用 `app/go.mod` 指定的 Go 版本，在 `app` 目录运行：

```bash
go test ./internal/modules/merchantfulfillment/... ./internal/bootstrap/merchantfulfillment ./internal/app/httpserver
```

这些测试仅使用临时 SQLite 数据库与公开示例数据，覆盖金额、退款复查、重复请求、并发重试、签名、防重放、正文上限和三种交付内容。

可选的跨语言测试用真实 Python 客户端、工作台发货运行器和库存模块访问临时 Go HTTP 服务，检查试运行不创建任务、不扣库存，卡密和资料正式交付，实物登记运单后回填，以及重复扫描和重启不重复交付。将 `UECB_TEST_PYTHON` 指向店小助虚拟环境的 Python，`UECB_TEST_BOT_ROOT` 指向店小助源码目录，再执行：

```bash
go test ./internal/modules/merchantfulfillment/integrationtest -run TestPythonConnector -v
```

这验证协议互通，不代表生产数据库、支付渠道或真实买家交付已经验证。
