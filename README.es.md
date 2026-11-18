# Posthorn

[简体中文](README.md) · [English](README.en.md) · [日本語](README.ja.md) · [한국어](README.ko.md) · Español

Posthorn es una pasarela de correo transaccional para aplicaciones autohospedadas. Recibe mensajes mediante formularios HTTP, una API JSON o SMTP y los envía a través de proveedores como Resend, Postmark, Brevo, Mailgun o Amazon SES. Puedes configurar reglas de enrutamiento, proveedores de respaldo y cuotas desde un único servicio.

El panel de administración está disponible en cinco idiomas. Permite gestionar proveedores y claves de API por proyecto, consultar el estado de los envíos y revisar la cola de reintentos. Posthorn guarda el historial en SQLite y cifra las credenciales de proveedores guardadas desde el panel.

## Probar con Docker Compose

Clona el repositorio y sigue la guía de Docker Compose para construir la imagen desde el código fuente, crear una clave de cliente e iniciar el servicio. La configuración de ejemplo escucha en la interfaz local. La primera prueba responde con `dry_run` y una vista previa del mensaje antes de habilitar el envío real. [Guía de Docker Compose](deploy/compose/README.md)

```bash
git clone https://github.com/cryotx/Posthorn.git
cd Posthorn/deploy/compose
```

Para habilitar el panel, genera un token independiente con `openssl rand -hex 32`, asígnalo a `POSTHORN_ADMIN_TOKEN` en tu archivo privado `.env` y aplica la configuración con `docker compose up -d`. Abre `/admin/login` en la dirección local del servicio y selecciona **Español**. En **Proyectos** puedes crear una clave de API para cada aplicación. Cópiala cuando aparezca y guárdala de forma segura porque solo se muestra una vez. Al reemplazarla, actualiza la aplicación de inmediato.

## Compilar con Go

Se necesita Go 1.25 o posterior. Desde la raíz del repositorio:

```bash
cd core
mkdir -p ../bin
go build -o ../bin/posthorn ./cmd/posthorn
../bin/posthorn version
```

Para integrar otras aplicaciones, consulta los ejemplos de llamadas HTTP y de configuración TOML. [API HTTP](docs/calling-posthorn.md) · [Configuraciones de ejemplo](examples/)

## Origen y licencia

Esta edición se basa en el proyecto original Posthorn y conserva la licencia Apache 2.0. [Proyecto original](https://github.com/craigmccaskill/posthorn) · [Licencia](LICENSE)
