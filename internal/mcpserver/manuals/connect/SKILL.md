---
name: piumy-connect
description: Use when you are an AI agent that needs to connect to a Piumy gateway — first time, o para reconectarte después de que las claves rotaron. Te deja conectado y recibiendo despachos; no te dice cómo contestar (eso es piumy-operator) ni cómo configurar el sistema (eso es piumy-orchestrator).
---

> Fuente de verdad de este manual — vive en el repo y se embebe en el binario (`get_manual` por MCP, ct-2026-08-05-0225). `.claude/skills/piumy-connect/SKILL.md` es una copia; editarla a ella no tiene efecto.

# Piumy — conectarse

Este manual te deja **conectado y recibiendo lo que te llega.** No es el manual de cómo contestar (`piumy-operator`) ni el de cómo configurar el sistema (`piumy-orchestrator`) — léelos aparte cuando termines esto.

Piumy despacha por **cAPI** — el protocolo de agentes externos de CleverCoder (antena, handshake, pinpass, el túnel cifrado por terminal). Ese protocolo no es de Piumy: es de CleverCoder, y **la skill `capi-protocol` es su fuente de verdad** — léela para el mecanismo completo (handshake, derivación de la key, formato del mensaje). Aquí abajo solo están las herramientas *de Piumy* que arman/leen esa conexión — no se redefine el protocolo.

Sigue los seis pasos en orden. No inventes un atajo: cada uno existe porque el anterior no alcanza solo.

## El procedimiento

### 1. ¿Piumy está instalado?

**Dónde se instala Piumy, en Windows:**

| Qué | Dónde |
|---|---|
| El programa | `%LOCALAPPDATA%\Piumy\Piumy.exe` |
| Su configuración | `%LOCALAPPDATA%\Piumy\piumy-config.json` |
| Sus datos y claves | `%LOCALAPPDATA%\Piumy\secrets\` |
| Acceso directo | Menú inicio → `Piumy` |

Si esa carpeta no existe, no hay nada corriendo. **El instalador se descarga de `https://github.com/chamilonster/Piumy/releases/latest`** — un único `.exe`, se instala con doble clic y no pide nada más. Hoy solo existe instalador para Windows.

Si el programa está pero no responde, arráncalo desde el menú inicio o ejecutando `%LOCALAPPDATA%\Piumy\Piumy.exe`. Es una aplicación de bandeja: al arrancar no abre ventana, se queda como ícono al lado del reloj.

### 2. ¿Está corriendo?

Los dos puertos que declara `agent-connect.json` (`mcp_url`, `rest_url` — paso 3) tienen que responder. Si no, lánzalo desde la carpeta de instalación (el acceso directo, o `Piumy.exe` a mano).

### 3. Lee `agent-connect.json`

Vive en la carpeta de datos de la instalación, junto a `status.json` — en una instalación estándar de Windows, `%LOCALAPPDATA%\Piumy\secrets\agent-connect.json`. Se reescribe entero en cada arranque; las claves pueden haber rotado desde la última vez que te conectaste.

```json
{
  "mcp_url": "http://127.0.0.1:8091/mcp",
  "rest_url": "http://127.0.0.1:8092",
  "mcp_key": "...",
  "rest_key": "..."
}
```

De aquí salen las dos URLs y las dos claves de acceso. **Nunca escribas una clave dentro de esta skill.** Un campo vacío (`""`) significa que esa clave no está configurada — no la inventes, sigue sin ella (`mcp_key`/`rest_key` vacíos son válidos en dev/LAN).

### 4. Enciende tu propia antena

`capi_power(action:"on", confirm:true)` → `capi_credentials()`.

Devuelve una línea cruda: `ip:puerto chat_id:<guid> pin:<pinpass>` — el formato exacto que define `capi-protocol` (léela si algo de esta línea no te cierra). Es tuya — la usas sin editar en el paso 5, dos veces si te toca ser secundario.

### 5. ¿Hay líder?

`GET <rest_url>/api/admin/capi-connector`, header `X-API-Key: <rest_key>`. Mira el campo `terminal_id`: vacío = no hay.

**No hay líder → eres el líder.** `POST <rest_url>/api/admin/capi-connector-line`, mismo header, body:

```json
{"line": "<la línea cruda del paso 4, tal cual>"}
```

**Hay líder → comprueba que esté vivo.** `POST <rest_url>/api/admin/capi-connector/test`, mismo header. Body vacío.

- Responde `{"ok":true,...}` → entras como secundario. `register_agent` por MCP (`mcp_url`, header `Authorization: Bearer <mcp_key>` + `X-Piumy-Terminal-Id: <tu chat_id del paso 4>`) con:
  ```json
  {"endpoint": "http://<ip:puerto del paso 4>",
   "antenna_terminal_id": "<tu chat_id del paso 4>",
   "pinpass": "<tu pin del paso 4>"}
  ```
  `X-Piumy-Terminal-Id` y `antenna_terminal_id` son el mismo valor — es lo que hace que un despacho futuro te encuentre a ti y no a otro agente.
- Responde `{"ok":false,...}` → el líder está muerto. Tomas su lugar: mismo `capi-connector-line` del caso "no hay líder", con TU línea del paso 4.

### La configuración MCP de Piumy — la que va por defecto

Esta es la entrada que tienes que dejar en tu cliente MCP. En una instalación estándar de Windows los dos primeros valores son siempre los mismos; los dos últimos son tuyos:

```json
{"mcpServers": {"piumy-gateway": {
  "type": "http",
  "url": "http://127.0.0.1:8091/mcp",
  "headers": {
    "Authorization": "Bearer <mcp_key de agent-connect.json>",
    "X-Piumy-Terminal-Id": "<tu chat_id del paso 4>"
  }}}}
```

**Los dos errores que dejan esto sin funcionar, y no se ven hasta que fallas:**

- **La clave.** Sale de `agent-connect.json` de **esta** instalación (paso 3) y **rota en cada arranque**. Una clave copiada de otra instalación, de un ejemplo, o de una configuración vieja devuelve **HTTP 401**, siempre. No hay clave "de desarrollo" que sirva en una instalación real.
- **El identificador.** Va tu `chat_id` del paso 4, **entero y tal cual** (`capi-<proyecto>-<agente>-<verificador>`). No pongas tu nombre, ni `principal`, ni un valor inventado: el despacho se entrega por ese campo, y con un valor que el gateway no reconoce no te llega nada — sin error visible.

Si estás dentro de CleverCoder, no edites el `.mcp.json` a mano (se regenera): usa `my_mcp_add` con `name:"piumy-gateway"` y este mismo objeto como `payload`. **Toma efecto al reabrir tu terminal**, no antes.

### 6. Verifica de verdad

`POST <rest_url>/api/admin/capi-ping`, header `X-API-Key: <rest_key>`. Body vacío si eres el líder; `{"agent_id": "<tu chat_id del paso 4>"}` si te registraste como secundario en el paso 5 — es la misma identidad que usaste ahí.

**La prueba es recibir el ping en tu propio terminal, no la respuesta HTTP del ping.** Que el POST haya devuelto `{"ok":true,...}` solo dice que el gateway logró inyectártelo — no que te haya llegado. Si no ves nada, la antena no está prendida, o el conector quedó mal registrado — repasa el paso 4/5, o `capi-protocol` si el problema es del handshake mismo (401, 404).

## Lo que tienes que tener claro, además de los pasos

### Conectado no es habilitado: sin un despacho activo no puedes actuar

Apenas te conectas, casi todas las herramientas te van a responder esto:

```
refused: no active dispatch for this terminal (default DENY) — call get_instructions first
```

**Eso no es una falla de tu conexión: es el sistema funcionando.** Un agente no puede escribirle a nadie por iniciativa propia. Solo puede actuar mientras atiende un mensaje que entró de verdad, y con el nonce de ese despacho.

Cómo distinguir una cosa de la otra sin perder tiempo:

- `get_status` **sí responde sin despacho** — es la puerta permitida. Si te contesta, estás bien conectado: mira `agent_connected` y `agents`. Si te contesta y las demás te rechazan, no toques nada más: solo estás esperando trabajo.
- Si `get_status` también falla, entonces sí es la conexión — vuelve al paso 3 (clave) y al paso 5 (identificador).

**Un despacho no se puede fabricar.** Nace de un mensaje real entrante de WhatsApp. El `capi-ping` del paso 6 comprueba el camino pero **no habilita**: su nonce no se registra en el gate, a propósito, para que un agente no pueda autoautorizarse. Si necesitas probar que puedes responder, pídele al dueño que le escriba al número — no busques la vuelta.

### Un rechazo del gate es la respuesta, no un obstáculo

Si `register_agent` te rechaza, si `capi-connector-line` te rechaza, si `capi-connector/test` no responde — esa es la respuesta. No lo rodees por otra vía, no le pidas a otro agente que lo haga por ti, no reintentes con datos distintos "a ver si pasa". Es la misma ley del manual del operador: el rechazo no se sortea.

### La API de administración es del dueño

`/api/admin/*` no es una puerta de servicio genérica: es la administración de **esta instalación en particular**, protegida por el `rest_key` de **esta instalación en particular**, leído de **su** `agent-connect.json`. Usarla para conectarte a tu propio gateway es el propósito; usarla para cualquier otra cosa no lo es.

### Armar un cuerpo con texto humano: nunca por la línea de comandos

Los `curl -d '...'` de este manual llevan solo ids, claves o literales fijos — son seguros tal cual. **En cuanto el cuerpo lleva texto que escribió una persona** (un mensaje, una regla, cualquier campo en lenguaje humano) pasarlo inline en la línea de comandos puede llegar roto, o perderse entero. No es un problema de acentos en español: en portugués o alemán se rompe igual, y en chino o árabe el texto desaparece — la terminal reemplaza cada caracter que no sabe convertir por `?`.

La causa: la terminal recodifica el argumento con su codificación local **antes** de que el proceso lo reciba, y esa codificación local casi nunca es UTF-8. Pasa igual mandando un JSON-RPC al MCP (`mcp_url`) que un body a la REST (`rest_url`) — no es un problema de un endpoint puntual, es de cómo se le pasa texto a cualquier proceso por argv. La solución no depende del idioma: nunca pases texto humano como argumento, siempre como archivo.

```bash
# ROMPE — el texto viaja por argv, la terminal lo recodifica
curl -d '{"text":"..."}' <url>

# ANDA — el texto viaja como bytes de archivo, no pasa por argv
curl --data-binary @cuerpo.json <url>   # cuerpo.json guardado en UTF-8
```

Probado (T29, `ct-2026-08-06-0140`): el mismo texto en español/portugués/alemán + chino + árabe, mandado de las dos formas contra un servidor de prueba — inline llegó con los acentos rotos y el chino/árabe reemplazados por `?`; por archivo llegó byte a byte idéntico, en las cinco lenguas.
