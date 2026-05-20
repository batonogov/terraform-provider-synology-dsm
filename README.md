# terraform-provider-synology-dsm

Terraform provider для управления [Synology DSM](https://www.synology.com/en-global/dsm) как корпоративным файловым облаком — Infrastructure as Code.

## Возможности

| Ресурс | Описание | Статус |
|--------|----------|--------|
| `dsm_user` | Управление пользователями | MVP |
| `dsm_group` | Управление группами | MVP |
| `dsm_shared_folder` | Общие папки | MVP |
| `dsm_share_permission` | Права доступа | Roadmap |
| `dsm_user_quota` | Квоты пользователей | Roadmap |

## Требования

- Terraform >= 1.0
- Go >= 1.26 (для разработки)
- [Task](https://taskfile.dev) (для сборки)

## Установка (локальная разработка)

```bash
git clone https://github.com/batonogov/terraform-provider-synology-dsm.git
cd terraform-provider-synology-dsm
task install
```

## Использование

```hcl
terraform {
  required_providers {
    dsm = {
      source  = "batonogov/dsm"
      version = "0.1.0"
    }
  }
}

provider "dsm" {
  host     = "https://diskstation:5001"
  username = "admin"
  password = var.dsm_password
  insecure = true  # для самоподписанных сертификатов
}

resource "dsm_user" "john" {
  name        = "john.doe"
  password    = var.user_password
  description = "John Doe - Engineering"
  email       = "john.doe@example.com"
  groups      = ["users"]
}

resource "dsm_group" "developers" {
  name        = "developers"
  description = "Development team"
}

resource "dsm_shared_folder" "team_data" {
  name               = "team-data"
  vol_path           = "/volume1"
  description        = "Team shared data"
  enable_recycle_bin = true
}
```

## Конфигурация провайдера

| Атрибут | Тип | Обязательный | Описание |
|---------|-----|-------------|----------|
| `host` | string | да | URL DSM (например `https://diskstation:5001`) |
| `username` | string | да | Имя администратора DSM |
| `password` | string | да | Пароль (sensitive) |
| `insecure` | bool | нет | Пропускать проверку TLS-сертификата |

Все параметры провайдера можно задать через переменные окружения:
- `SYNOLOGY_DSM_HOST`
- `SYNOLOGY_DSM_USERNAME`
- `SYNOLOGY_DSM_PASSWORD`

## Ресурс: dsm_user

| Атрибут | Тип | Обязательный | Вычисляемый | Описание |
|---------|-----|-------------|-------------|----------|
| `id` | string | - | да | Идентификатор (username) |
| `name` | string | да | - | Имя пользователя |
| `password` | string | да | - | Пароль (sensitive) |
| `description` | string | нет | - | Описание |
| `email` | string | нет | - | Email |
| `disabled` | bool | нет | да | Отключён (default: false) |
| `groups` | list(string) | нет | - | Список групп |
| `uid` | int | - | да | UID (read-only) |

## Ресурс: dsm_group

| Атрибут | Тип | Обязательный | Вычисляемый | Описание |
|---------|-----|-------------|-------------|----------|
| `id` | string | - | да | Идентификатор (group name) |
| `name` | string | да | - | Имя группы |
| `description` | string | нет | - | Описание |
| `gid` | int | - | да | GID (read-only) |

### Import

```bash
terraform import dsm_group.developers developers
```

## Ресурс: dsm_shared_folder

| Атрибут | Тип | Обязательный | Вычисляемый | Описание |
|---------|-----|-------------|-------------|----------|
| `id` | string | - | да | Идентификатор (name) |
| `name` | string | да | - | Имя общей папки |
| `vol_path` | string | да | - | Путь к тому (например `/volume1`) |
| `description` | string | нет | - | Описание |
| `hidden` | bool | нет | да | Скрыть из сети (default: false) |
| `enable_recycle_bin` | bool | нет | да | Корзина (default: true) |
| `uuid` | string | - | да | UUID (read-only) |

### Import

```bash
terraform import dsm_shared_folder.team_data team-data
```

## Data source: dsm_shared_folder

| Атрибут | Тип | Обязательный | Вычисляемый | Описание |
|---------|-----|-------------|-------------|----------|
| `id` | string | - | да | Идентификатор (name) |
| `name` | string | да | - | Имя общей папки |
| `description` | string | - | да | Описание |
| `vol_path` | string | - | да | Путь к тому |
| `uuid` | string | - | да | UUID (read-only) |

## Data source: dsm_group

### Import

```bash
terraform import dsm_user.john john.doe
```

## Разработка

```bash
task build       # Сборка
task test        # Тесты
task lint        # Линт
task install     # Установка в локальное окружение Terraform
task clean       # Очистка артефактов
```

## Лицензия

[MIT](LICENSE)
