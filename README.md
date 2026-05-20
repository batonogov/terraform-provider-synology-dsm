# terraform-provider-synology-dsm

Terraform provider для управления [Synology DSM](https://www.synology.com/en-global/dsm) как корпоративным файловым облаком — Infrastructure as Code.

## Возможности

| Ресурс | Описание | Статус |
|--------|----------|--------|
| `dsm_user` | Управление пользователями | MVP |
| `dsm_group` | Управление группами | MVP |
| `dsm_shared_folder` | Общие папки | Roadmap |
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

## Data source: dsm_group

| Атрибут | Тип | Обязательный | Вычисляемый | Описание |
|---------|-----|-------------|-------------|----------|
| `id` | string | - | да | Идентификатор (group name) |
| `name` | string | да | - | Имя группы |
| `description` | string | - | да | Описание |
| `gid` | int | - | да | GID (read-only) |

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

[MPL-2.0](LICENSE)
