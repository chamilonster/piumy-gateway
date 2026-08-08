# Changelog

Todos los cambios notables de piumy-gateway. Formato basado en
[Keep a Changelog](https://keepachangelog.com/es/). Lo más reciente arriba.
Se actualiza en cada deploy/build junto con el relanzamiento del gateway.

---

## Sin publicar

---

## 0.1.11 — 2026-08-08

### Added
- **`send_to_boss`: un agente le puede escribir al dueño por WhatsApp sin
  tener un despacho activo** (T39, ct-2026-08-08-1619, pedido del dueño:
  "que tal una herramienta: send to boss en el mcp, que pueda usarlo
  cualqueira que tenga el mcp?" — con la enmienda que define el diseño:
  "pero que el agente se identifique"). Antes, un terminal sin despacho no
  podía hacer nada — un agente en medio de una tarea ("avisame cuando
  termines") no tenía forma de llegar al dueño. La herramienta no acepta
  destino (siempre va a los chats marcados dueño) ni identidad declarada
  (se resuelve del registro, cruzando la conexión — un terminal_id no
  registrado recibe error explícito y no envía nada); el mensaje sale
  firmado `[nombre] texto` y pasa por la misma cola con freno anti-ban de
  siempre, nunca directo.

---

## 0.1.10 — 2026-08-08

### Fixed
- **La señal de "no se pudo descifrar" quedaba solo en un log que nadie ve
  en producción** (T35, ct-2026-08-08-1258). `handleRetryReceipt` detecta
  correctamente el retry receipt de WhatsApp (la única señal observable de
  que un mensaje nuestro llegó ilegible al destinatario, ver 0.1.9 más
  abajo), pero antes solo hacía `log.Printf` — sin `log.SetOutput` a
  archivo y compilado `-H windowsgui` (sin consola), esa línea se tiraba.
  Ahora la señal se persiste (`messages.decrypt_retry_at`, columna nueva)
  y se puede consultar desde `GET /api/messages`, no solo verse por
  casualidad en una captura de pantalla del contacto. **(T36,
  ct-2026-08-08-1312)** Ese primer arreglo tenía un hueco que le pegaba
  justo al caso real que originó todo: para un chat bajo LID, el mensaje se
  guarda con el chat_jid ya resuelto a número (`resolveChatJID`), pero la
  marca buscaba por el `@lid` crudo del receipt — no encontraba la fila y,
  como cero filas no es error, fallaba en silencio, otra vez. `handleMessage`
  y `handleRetryReceipt` ahora resuelven el chat_jid con la misma función
  (`resolveChatJID`, bajada a `types.MessageSource` para servir a los dos),
  así el guardado y la marca no pueden volver a divergir.

### Added
- **La versión de piumy-gateway ahora se ve desde el tray** (T37,
  ct-2026-08-08-1433, pedido del dueño: "quiero que el tray diga la version
  de piumy"). Piumy corre sin ventana (`-H windowsgui`) — el ícono del tray
  es lo único visible del proceso, y hasta ahora no había forma de saber qué
  versión estaba corriendo sin abrir el dashboard. El menú del ícono ahora
  trae, como primer ítem y deshabilitado, `Piumy Gateway <versión>` —
  leído de `internal/version.Version`, la fuente única (nunca escrito a
  mano). El tooltip/título del ícono quedan sin tocar, a pedido explícito
  del dueño ("en el tray en el menú, no al pasar el mouse").

---

## 0.1.9 — 2026-08-08

### Fixed
- **Los mensajes salientes podían llegar imposibles de descifrar**
  (ct-2026-08-07/08, caso real: un contacto real recibió un mensaje del
  gateway y nunca le llegó el texto — WhatsApp le mostró "Esperando
  mensaje" en su lugar, y nadie del lado del gateway se enteró hasta que
  ella mandó una captura de pantalla un día después). Causa confirmada
  leyendo la sesión real en modo solo-lectura: la cuenta **tiene** un LID
  asignado pero `lid_migration_ts` está en **0** — con la versión de
  whatsmeow que corría hasta ahora (`v0.0.0-20260709092057`), ese flag en
  0 hacía que los mensajes directos se siguieran direccionando por
  **número de teléfono** en vez del LID, aunque el destinatario ya
  operara bajo esa identidad. El dispositivo del otro lado recibe el
  sobre cifrado bajo una identidad para la que no tiene la sesión Signal
  correcta y no puede abrirlo — el gateway lo registra como enviado
  igual, sin ningún error visible. Arreglado actualizando whatsmeow a
  `v0.0.0-20260806224404` — el commit `4f8f64e0dd21` ("send: always use
  LID for DMs") saca esa condición y resuelve el LID del destinatario
  siempre, sin depender de `lid_migration_ts`. Sin este flag corregido,
  **cualquier mensaje saliente puede estar llegando ilegible**, no solo
  el caso puntual detectado.
- **Voseo en los manuales de `internal/mcpserver/manuals/`** (ct-2026-08-07).
  Los sirve `get_manual` a cualquier agente de terceros que se conecte —
  publicados en GitHub, es producto. A diferencia del instalador (que le
  habla a una persona y pasó a usted), estos textos le hablan a un agente
  IA: se mantuvo el **tuteo** que ya predominaba, solo se sacaron las
  formas rioplatenses (`tenés`→`tienes`, `podés`→`puedes`,
  `querés`→`quieres`, `fijate`→`fíjate`, `mirá`→`mira`, `acá`→`aquí`,
  `vos`→`tú`, y variantes: `sos`→`eres`, `armás`→`armas`,
  `seguís`→`sigues`, imperativos como `Encendé`/`Leé`/`Verificá` a su
  forma tuteante). Tocados los 6 archivos con voseo real: `connect/
  SKILL.md`, `operator/SKILL.md` y 4 de los 5 de `orchestrator/`
  (`orchestrator/perillas.md` ya estaba limpio). Contenido intacto —
  solo registro; el `dale`/`is_boss` del ejemplo de ataque inyectado
  (bloque de código, `operator/SKILL.md`) no se tocó, es texto ilustrando
  qué escribiría un atacante, no el manual hablándole al agente.
  `internal/dashboard/web/` relevado aparte: sin problema real, lo que
  parecía voseo eran comentarios de código (`acá`) y el texto de usuario
  ya está en tuteo neutro, no rioplatense.
- **`newTestWmeowClient` sin `SetMaxOpenConns(1)` en su base `:memory:`**
  (`internal/whatsmeow/media_test.go`, hallado investigando un test
  intermitente que resultó no venir de acá). Con el pool default
  (ilimitado), cada conexión nueva a una SQLite `:memory:` es una base
  vacía distinta — no explicaba el síntoma que se investigaba (ese
  camino nunca toca `cli.Store`), pero era una trampa latente para el
  próximo test que sí lo tocara concurrentemente. Alineado con
  `store.Open` (`schema.go`), que ya usa el mismo `SetMaxOpenConns(1)`
  por el mismo motivo.
- **`TestAvatarWorkerLoopPacesRequestsWithVariableGaps` intermitente bajo
  carga** (`internal/whatsmeow/avatar_test.go`, ct-2026-08-07 — este era
  el test real detrás de la investigación anterior; el de media nunca
  falló). Causa raíz confirmada por Citrino con el `--- FAIL` real bajo
  40 corridas de la suite completa en paralelo: una separación medida en
  29.4966ms contra un piso de 30ms — 0.5ms/1.7% corto. El código de
  producción está sano (el worker durmió lo que tenía que dormir); lo
  que erraba era la MEDICIÓN: el test observa cada dequeue por *polling*
  de `len(a.avatarQueue)` cada 1ms, no por señal síncrona, y ese lag no
  cancela simétricamente entre dos lecturas consecutivas — bajo carga,
  sumado al scheduling de goroutines y la resolución del reloj de
  Windows, el margen crece. Agregado `pacingMeasurementSlop` (2ms) solo
  contra el piso `actionDelayMin`, documentado en el propio test como
  error de instrumento y no permiso para que el pacing afloje. **Sin
  tocar** la validación de que las separaciones son variables (nunca la
  misma dos veces) — esa sigue exactamente igual de estricta. Verificado
  con los números reales de Citrino (29.4966ms pasa con el margen nuevo;
  un pacing genuinamente roto, 5ms o menos, sigue fallando) y con el
  mismo método de reproducción bajo carga: 10 corridas completas de la
  suite en paralelo (57-149s cada una en vez de los ~7s normales) más
  intentos dedicados del test — sin una sola falla del test corregido.
- **`TestControllerStopWaitsForInFlightOutboxSend` intermitente bajo carga**
  (`internal/corepipeline/controller_test.go`, ct-2026-08-07 — reproducido
  por Citrino con dos síntomas distintos: `outbox Send was never entered`
  y `sql: database is closed`). **No es una carrera de producción** —
  `Controller.Stop()`/`Pipeline.Run()` siguen uniendo `outboxLoop`
  correctamente, reverificado bajo la misma carga. El bug estaba en el
  propio test: a diferencia de los demás tests de este archivo, no tenía
  un `defer c.Stop()` incondicional — si el primer `t.Fatal` (el timeout
  de `sendStarted`, que bajo carga extrema puede tardar minutos en
  cumplirse) disparaba, el `Goexit` saltaba directo por encima del
  `c.Stop()` explícito de más abajo, dejando el Controller corriendo sin
  parar mientras el `defer st.Close()` ya registrado cerraba la base — de
  ahí el `database is closed` repetido, un `processOutbox` huérfano
  reintentando cada 5ms contra una base cerrada. Arreglado con
  `sync.OnceFunc`: el `defer` de limpieza y el `Stop()` explícito del
  medio del test son ahora la MISMA llamada — cualquiera de los dos
  caminos que dispare primero, el otro espera a que termine de verdad
  (contrato propio de `sync.Once`), en vez de pisarlo con un no-op
  idempotente. Reproducido de forma confiable con 15 corridas paralelas de
  `go test ./...` (15/15 fallaban antes del fix) y verificado sin fallas
  con el mismo método: 109 corridas dedicadas + las 15 corridas completas
  de la suite, todas en verde, bajo la carga que antes lo hacía fallar
  siempre.
- **`TestFloodGuardThrottlesGeneralCalls` intermitente bajo carga**
  (`internal/mcpserver/floodguard_test.go`, ct-2026-08-07 — visto fallar
  por timeout, 30.8s, bajo carga pesada). **No es un bug del flood guard**
  — su token bucket (`internal/mcpguard`) hace exactamente lo que tiene
  que hacer: si pasan 30 segundos reales entre dos llamadas con
  `RatePerMin: 2`, rellena un token, permite la siguiente y NO la
  bloquea — es la defensa real funcionando, no rota. El test asumía que
  sus 3 llamadas quedarían pegadas en el tiempo real, algo que ninguna
  cantidad de reintentos locales puede garantizar bajo contención extrema
  del scheduler del SO (a diferencia del de avatares, acá el retraso es
  externo al test, no una imprecisión de su propia medición). Agregado
  `Guard.SetClock` (`internal/mcpguard/mcpguard.go`) — inyecta la fuente
  de tiempo de `Check`, test-only, producción sigue con `time.Now` por
  default. El test ahora fija un reloj congelado: las 3 llamadas quedan
  en el MISMO instante desde el punto de vista del bucket, sin depender
  en absoluto del tiempo real transcurrido — a diferencia de los dos
  flakies anteriores, esto no reduce la probabilidad de fallo bajo carga,
  la elimina por completo (verificado con una prueba sintética: el mismo
  bucket con reloj congelado ignora un `time.Sleep` real de 200ms entre
  llamadas, y sigue rellenando correctamente si el reloj inyectado se
  adelanta de verdad). **No logré reproducir esta falla específica hoy**
  pese a ~100 corridas bajo dos tandas de carga creciente (hasta 25
  suites completas en paralelo) — a diferencia de los casos anteriores,
  esto no fue necesario para confiar en el arreglo: la aritmética del
  token bucket es exacta, no probabilística, y confirmé el mismo
  mecanismo de raíz en un test hermano (`TestCircuitBreakerBlocksAfterThreshold`,
  `internal/mcpguard`) que sí falló bajo carga pesada en esta sesión.
- **`TestCircuitBreakerBlocksAfterThreshold` — mismo arreglo** (`internal/
  mcpguard/mcpguard_test.go`, ct-2026-08-07). Gemelo exacto del flaky de
  `TestFloodGuardThrottlesGeneralCalls` de arriba — mismo `Guard.Check`
  llamado varias veces seguidas sin control de reloj, y este SÍ se vio
  fallar bajo carga real en esta sesión. Mismo `Guard.SetClock`, mismo
  reloj congelado.
  **Cierre de la serie de tests intermitentes** (T33, T19, T14, avatares,
  corepipeline, flood guard, circuit breaker): de acá en más, se arregla
  lo que falla en una corrida normal de la suite. Lo que solo aparece
  forzando 20-25 corridas de `go test ./...` en paralelo (peleando por
  CPU entre sí) no es un test roto — es un entorno que nadie usa; bajo
  carga suficiente, cualquier test que toque tiempo real puede fallar, y
  perseguir eso no tiene fondo. `TestControllerStopWaitsForInFlightOutboxSend`
  (con su propio timeout de 2s, no la causa ya arreglada),
  `TestControllerRunsPipelineEndToEnd` y `TestPrivilegedToolsRefuseNonBoss`
  cayeron solo bajo esa carga extrema (25 suites en paralelo) — quedan
  anotados, no perseguidos, hasta que alguno moleste en una corrida normal.

### Added
- **Rastro de mensajes ilegibles — `types.ReceiptTypeRetry`** (ct-2026-08-07,
  caso real: un contacto real recibió un mensaje del gateway que le llegó
  ilegible — WhatsApp le mostró "Esperando mensaje" en vez del texto — y
  el gateway nunca se enteró; se supo un día después, por una captura de
  pantalla). WhatsApp ya manda esta señal (`types.ReceiptTypeRetry`,
  documentado por la propia librería: *"the message was delivered to the
  device, but decrypting the message failed"*) y whatsmeow ya la
  despachaba como `*events.Receipt` — el switch de `handleEvent`
  (`inbound.go`) nunca tenía un caso para `*events.Receipt`, así que se
  perdía en silencio. Ahora deja un log claro (`whatsmeow: mensaje a
  %s (id %s) llegó al dispositivo pero no se pudo descifrar...`) filtrado
  a **solo** el tipo retry — `*events.Receipt` llega para todo tipo de
  acuse (entregado, leído, reproducido), y logueoar los demás habría
  vuelto a esconder la señal real en ruido. **Solo observa — no reintenta,
  no reenvía, no cambia el direccionamiento del envío.** Esa decisión
  (actualizar whatsmeow, rama `whatsmeow-update-prep` aparte) es de
  Citrino/el boss. No se persiste contra la base: el schema de `messages`
  no tiene una columna para esto hoy y agregar una es decisión de Citrino,
  no tomada acá.

---

## 0.1.8 — 2026-08-07

### Fixed
- **`onExit` del tray no cerraba nada** (ct-2026-08-07). El segundo callback
  de `systray.Run` en `tray_windows.go` estaba vacío — `WM_CLOSE`/
  `WM_ENDSESSION` (apagado de Windows, o un cierre externo sin `/F`) llegan
  hasta `fyne.io/systray` y de ahí no pasaban a ningún lado: el proceso de
  Piumy podía seguir corriendo después de que el ícono de bandeja
  desapareciera. Ahora `onExit` llama a `stop()`, el mismo `CancelFunc` que
  ya usan "Salir" y `ctx.Done()` (idempotente, sin caminos nuevos).
  Verificado con una instancia de prueba propia (nunca la de producción):
  `taskkill` sin `/F` → cierre ordenado completo (`"piumy-gateway shutting
  down"` seguido de `"piumy-gateway stopped"`) en ~600ms.
- **Voseo en los textos de error del instalador** (ct-2026-08-07). Los
  mensajes que `installer/windows/piumy.iss` le muestra al usuario final
  (`Result`/`Log`/`MsgBox` de claves existentes ilegibles, instancia
  corriendo sin poder cerrarse, clave repetida que no coincide, fallo al
  crear `piumy-config.json`, desinstalación) estaban en voseo rioplatense
  (`Cerrá`, `volvé`, `borrá`, `querés`, `Cerrala`, `Ábrelo`...) — Piumy se
  publica a terceros de cualquier país hispanohablante. Pasados a
  tratamiento de **usted**, mismo contenido, sin reescribir nada. La
  página de la clave del tablero (`CreateInputQueryPage`) tenía el mismo
  problema pero en tuteo (`Elige`, `vas a entrar`, `tu WhatsApp`,
  `puedes`, `Repite`) — una pasada anterior las había sacado del voseo
  sin llegar a usted; corregido también. `WizardForm.WelcomeLabel1/2` NO
  se tocaron: el comentario que las precede las marca como texto verbatim
  del boss.

---

## 0.1.7 — 2026-08-07

### Fixed
- **El instalador no cerraba el Piumy corriendo** (`ct-2026-08-07-0415-t34`,
  bug reportado por el boss con captura). Dos fallas, no una: en modo
  interactivo pedía al usuario cerrar la app a mano — y Piumy es de bandeja,
  sin ventana, así que ni sabía cómo. En modo desatendido (`/VERYSILENT
  /SUPPRESSMSGBOXES`) el diálogo de `AppMutex` (suprimido, respuesta por
  defecto Cancelar) disparaba un `EAbort` — Inno terminaba con exit code 1,
  pero **sin instalar nada**: el peor de los dos mundos, ya publicado en
  0.1.6. Auditando el registro apareció el alcance real: la última
  instalación que el instalador completó de verdad acá fue **0.1.3** — desde
  entonces cada actualización de binario fue manual (los `Piumy.exe.bak-pre-*`
  en la carpeta real lo confirman).
  `CloseApplications=yes` (RestartManager) no alcanza contra una app de
  bandeja sin ventana principal — confirmado con un decoy `-H windowsgui`
  que solo toma el mutex y duerme, sin código de producto real de por medio:
  con RestartManager solo, el decoy queda corriendo y el diálogo igual
  aparece. El cierre es explícito (`taskkill /F` por nombre de imagen,
  verificado releyendo `tasklist` — no el exit code de `taskkill`, que varía
  entre versiones de Windows) y corre en `InitializeSetup`, no en
  `PrepareToInstall`: un log real (`/LOG=`) mostró que el propio diálogo de
  Inno dispara y aborta ANTES de que `PrepareToInstall` llegue a
  ejecutarse — `InitializeSetup` es el primer código del script, corre antes
  que cualquier chequeo interno de Inno. Si no logra cerrar la instancia
  corriendo, el instalador ahora aborta con exit code distinto de cero —
  nunca más "0 sin instalar nada". Piumy se relanza solo al terminar (ya lo
  hacía, sin cambios).
  Probado contra un decoy propio, nunca contra la instalación real: modo
  silencioso — cierra el proceso viejo, instala, `DisplayVersion` en el
  registro pasa de la versión vieja a la nueva (no queda pegado), relanza,
  exit 0; modo interactivo — ningún diálogo aparece (confirmado por título
  de ventana, va directo al wizard normal); camino de falla — exit code
  distinto de cero confirmado forzando el chequeo de verificación.
- **T33 — actuar sobre otro chat dejaba el mensaje del dueño sin cerrar**
  (`ct-2026-08-06-1526`, caso real en vivo: el boss ordenó por WhatsApp
  escribirle a un tercer número, cambiarle las reglas y anotarle
  memoria/contexto — su propio mensaje le llegó dos veces).
  `send_message`/`draft` marcaban `MarkHandledBefore(to, ...)` — solo el
  chat DESTINO. Cuando el despacho que se está atendiendo es un chat
  distinto del destino, ese despacho nunca se marcaba, quedaba pendiente
  y el sweep lo re-despachaba con un nonce nuevo. `silent_act` nunca tuvo
  este bug (no tiene `to`, siempre marca el chat del despacho).
  - **No es un bug nuevo de T31** — corrección registrada en
    `docs/T33-DIAGRAMA-CERRAR-DESPACHO-OTRO-CHAT.md`: los bypasses de
    terminal-principal y despacho-boss en `levelGateMiddleware` ya
    permitían apuntar a otro chat antes de T31; T31 solo volvió eso el
    caso cotidiano, no lo creó.
  - Recorrido completo antes de codear (grep exhaustivo de
    `gate.Consume`/`MarkHandledBefore`, 4 call sites en todo
    `internal/mcpserver`): `approve_draft` verificado sano (marca el
    chat del borrador, nunca toca el turno propio, a propósito).
    Ninguna otra tool (`set_chat_rules`, `set_chat_memory`,
    `set_chat_context`, `set_chat_status`, `set_chat_active`,
    `set_mode`, `escalate`, `claim_chat`, `release_chat`,
    `mark_handled`, `resolve_chat`) toca el turno — nunca lo tocó. Si el
    turno entero de un agente es una de estas, el despacho queda
    atado-sin-consumir hasta `DispatchStaleAfter` (15 min) — y el
    **terminal entero** queda bloqueado ese tiempo, no solo ese chat.
    Decisión (con Citrino): no cerrar automático ahí — arreglado por
    skill, no por código.
  - Fix: `markDispatchChatIfDifferent` (`send.go`), un helper compartido
    entre los 3 call sites reales — cierra también el chat del despacho
    activo cuando difiere del destino, usando `active.BurstMaxTS` (nunca
    `now`, no marca de más). No-op en el caso de siempre (mismo chat).
  - Skill del operador (`internal/mcpserver/manuals/operator/SKILL.md`)
    corregida: tres afirmaciones que el código no respaldaba (una
    contradecía a otra línea del mismo documento — mismo patrón que el
    cifrado de T28), más la instrucción explícita del costo real de no
    cerrar un turno puramente administrativo.
  - Tests: `TestSendMessageToAnotherChatAlsoClosesDispatchChat`/
    `TestDraftToAnotherChatAlsoClosesDispatchChat` (confirmados que
    fallan sin el fix, antes de darlos por buenos),
    `TestSendMessageToAnotherChatDoesNotMarkDispatchMessagesAfterBurst`,
    `TestSendMessageSameChatDoesNotDoubleMark`.
- **T19 — el freno de emergencia ya sobrevive un reinicio**
  (`ct-2026-08-05-1249`, descubierto probando la instancia con sesión
  real — T10). `governor.SetKill`/`state.Muted` vivían solo en memoria:
  un reinicio (corte de luz, un update, un crash — no hipotético, le
  pasó al PC del boss: hibernó y el gateway se cayó) soltaba el freno en
  silencio. Si en ese momento estaba frenado por una razón real, el
  gateway volvía mandando exactamente cuando menos había que hacerlo.
  - `set_kill_switch` (MCP y `POST /api/admin/kill`) ahora también
    persiste a `store.SettingKillSwitch`, ANTES de aplicar el efecto en
    vivo — best-effort, logueado, nunca bloquea el freno en sí por un
    problema de disco.
  - `main.go`'s `restoreKillSwitch` (nuevo) relee esa marca al arrancar y
    aplica **las dos mitades juntas** (`governor.SetKill(true)` +
    `state.SetMuted(true)`) — llamado justo después de que existen
    `gov`/`sm`, muchísimo antes de `ctrl.Start()` (el único call que
    puede hacer que el pipeline mande algo). El orden es la parte que
    importaba: si arranca mandando y recién después lee el ajuste, ya
    salió lo que no debía.
  - El tablero ya mostraba el freno puesto en vivo (badge "⛔ kill" +
    carita en mood `muted`) — con el freno restaurado correctamente
    desde el arranque, esos mismos indicadores ahora también reflejan un
    freno que sobrevivió un reinicio, sin UI nueva.
  - Verificado con el binario real: `store` sembrado con el freno puesto,
    reinicio, `GET /api/status` confirmado devolviendo
    `governor_killed: true` / `muted: true` desde el primer poll.
  - Tests: `main_test.go` (`TestRestoreKillSwitch*`, la restauración en
    sí) + `corepipeline/outbox_test.go`'s
    `TestKillSwitchSurvivesRestartAndReallyBlocksSending` — la prueba
    explícitamente pedida de que un freno restaurado bloquea un envío
    REAL, no solo que las banderas queden en `true`. Extendidos también
    `TestSetKillSwitchFlipsGovernorAndState` (mcpserver) y
    `TestSetKillSwitchEndpoint` (restapi) para cubrir la persistencia.
- **T18** (`ct-2026-08-05-1243`, el boss: su bandeja mezclaba ~1351 chats —
  gente que le escribió junto a números que solo aparecieron en un grupo)
  — `store.ChatOrigin` solo miraba mensajes ENTRANTES
  (`from_me=0`): un chat que el dueño inició y nadie contestó caía en
  `group_discovered`/`synced_contact` en vez de `inbound_spoke`, aunque
  sea una conversación real. Reproducido con 3 casos concretos antes de
  codear. Fix: reusa `realMessageSQL` (misma constante que
  `ChatJIDsWithMessages` ya usa) sin filtro de dirección — de paso quedó
  excluido el ruido de protocolo (recibos/reacciones sin texto ni tipo)
  que la consulta original tampoco filtraba. El valor `"inbound_spoke"` se
  mantuvo sin renombrar (viaja por `list_chats`/`get_chat`/
  `decision-policy.md`) — se ajustaron esas 3 descripciones para decir lo
  que el criterio significa de verdad, y remiten a `last_speaker` para
  "me toca responder".
  - `chatOut.Origin` expuesto en `GET /api/chats` — ya se calculaba en
    cada `ListChats` (`enrichChat`) y se descontaba antes de esto.
  - La pestaña Chats del tablero filtra por `origin === "inbound_spoke"`
    (`app.js#isRealConversation`) en vez de `has_messages`.
  - **Hallazgo sin resolver, flagueado a Citrino:** `chat_groups` (la
    tabla que lee `group_discovered`) no tiene ningún escritor en código
    de producción — solo tests. El sync real de grupos escribe
    `group_members` (tabla hermana) a propósito, nunca `chat_groups`.
    `group_discovered` no dispara hoy en producción, y `get_chat_groups`
    (MCP) siempre devuelve vacío. La implementación paralela por
    `group_members` que oculta números-solo-de-grupo en `handleChats`
    quedó SIN TOCAR — es la única de las dos que funciona hoy; borrarla
    antes de resolver `chat_groups` habría sido peor que dejarla.
  - `docs/T18-DIAGRAMA-SEPARAR-POR-ORIGEN.md` con el detalle completo;
    `docs/MANUAL.md` documenta el hallazgo de `chat_groups` y distingue
    los tres conceptos de "origen" que conviven en el código (pedido de
    Citrino).
- **T17 Parte 1** (`ct-2026-08-05-1240`, el boss mandó capturas: la
  cabecera decía "(sin nombre)" sobre un número real) — `recordOwnIdentity`
  pisaba `state.OwnName` SIN CONDICIÓN en cada reconexión; cuando
  `client.Store.PushName` volvía vacío (confirmado contra la librería
  whatsmeow: esa mutación de appstate puede no repetirse nunca para una
  cuenta ya asentada, y no hay ninguna llamada consultable para pedirlo de
  nuevo), borraba cualquier nombre ya conocido — no un parpadeo al
  arrancar, el estado permanente. Reproducido a nivel código antes de
  tocar nada (`seedFakeDevice`, sin pareo real). Fix: `recordOwnIdentity`
  solo pisa `OwnName` cuando el valor nuevo no es vacío (mismo criterio
  que `TouchChat` ya aplica a `chats.name`); `state.NewManager` siembra
  `OwnName`/`OwnJID` (solo esos dos campos, nunca el resto del `Status`)
  desde un `status.json` existente al arrancar.
  - La hipótesis original ("arranca vacío, el archivo no se relee") no
    explicaba por completo lo que vio el boss — `OwnJID`/`OwnName` los
    escribe la misma llamada, así que un arranque en blanco dejaría los
    dos vacíos, no solo el nombre. La causa real es la de arriba.

### Added
- **Sello de versión en `get_manual`** (ct-2026-08-07, motivado por el
  rescate de contenido solo-en-copias: hasta ahora no había forma de saber
  si un manual leído era el de la versión que corre). Cada manual servido
  por `get_manual` termina con `<!-- piumy-skill-version: X.Y.Z -->`,
  agregado por `manualWithVersionStamp` al responder — nunca escrito en el
  `.md` embebido, lee `version.Version` en vivo en cada llamada, así nunca
  puede desincronizarse del binario que lo sirve.
- **T17 Parte 3 — avatares** (`ct-2026-08-05-1240`, sub-cambio partido
  aparte con OK de Citrino). Funcionalidad nueva y delicada: pedir fotos
  de perfil es actividad hacia WhatsApp, cuenta para el anti-ban igual
  que cualquier acción server-facing — 719 números/591 contactos hacen
  que un barrido masivo sea exactamente el patrón que tira una cuenta.
  - **Nunca un sweep.** `whatsmeow.Adapter.RequestAvatar(jid)` solo se
    llama desde `restapi` para un chat que el tablero está mostrando
    ahora mismo (cabecera o una fila visible de la lista) — nunca sobre
    todos los números conocidos.
  - **Bajo demanda, pero con cola paceada.** Sin la cola, "bajo demanda"
    con varios chats visibles a la vez sería una ráfaga disfrazada —
    `avatarWorkerLoop` drena un jid a la vez, espaciado con el mismo
    `governor.DelayWindow` que ya pacea el backfill de contactos/media
    (`actionDelay()`).
  - **El chequeo "¿cambió?" es gratis del lado del protocolo.**
    `GetProfilePictureInfo` con el `ExistingID` cacheado devuelve
    `(nil, nil)` sin transferir bytes cuando la foto no cambió —
    verificado contra la librería whatsmeow vendorizada. El riesgo
    anti-ban es la frecuencia de PREGUNTAR, no el peso de bajar.
  - **Ventana de re-chequeo aleatoria, nunca fija.** Corrección de
    Citrino sobre el primer borrador (que proponía "cada 7 días" liso):
    un intervalo fijo es un patrón, y los patrones son lo que se
    detecta. `avatarRecheckWindow()` sortea (mismo mecanismo
    `governor.DelayWindow.Random()`) una ventana de 3-9 días por jid, en
    cada chequeo — nunca el mismo offset dos veces para el mismo número.
    Override en caliente vía `store.SettingAvatarRecheckMin/Max`
    (`PIUMY_AVATAR_RECHECK_MIN/MAX`).
  - **Cache en disco** (`store.Avatar`, tabla `avatars` — deliberadamente
    fuera de `resetTables`, es un cache de WhatsApp, no historial del
    boss). Confirmado-sin-foto borra el archivo cacheado (la persona
    pudo haberla sacado); cualquier otro error bumpea el próximo chequeo
    sin tocar el cache existente.
  - **`GET /api/avatar?jid=`** sirve los bytes cacheados (mismo patrón
    binario que `GET /api/media`) y dispara el chequeo paceado de paso,
    sin esperarlo — sin cache, 404 inmediato.
  - **Sin foto → iniciales**, nunca un cuadrado vacío ni un ícono roto
    (`app.js#buildAvatar`, cabecera + lista de chats).
  - **Evidencia real del pacing** (pedido explícito, "es la parte que
    puede costar una cuenta"): `TestAvatarWorkerLoopPacesRequestsWithVariableGaps`
    drena la cola con el loop real y mide separaciones reales entre
    pedidos — nunca por debajo del mínimo configurado, nunca repetidas.
  - `docs/T17-DIAGRAMA-NOMBRE-Y-AVATAR.md` actualizado con el detalle
    completo de esta parte.

### Changed
- **T32** (`ct-2026-08-06-1109`, acuerdo con el líder de CleverCoder,
  implementado del otro lado en v1.6.68.191) — el handshake de cAPI ya no
  colapsa tres motivos de "no entrega" en un único error genérico.
  CleverCoder ahora distingue: `antenna_off`/`position_empty`
  (TRANSITORIO — el id es válido, no hay nadie ahí ahora mismo) vs.
  `terminal_gone` (PERMANENTE — no corresponde a nada, nunca va a
  existir). El despacho actúa distinto según cuál:
  - `antenna_off`/`position_empty` → vuelve a la cola, igual que antes de
    este cambio (reintento cada sweep, no consume el presupuesto de
    redespacho, no da de baja el registro del agente).
  - `terminal_gone` → `CleverInjector` descarta su propia credencial
    (`markDead`, nuevo — `Configured()` pasa a `false`) y `dispatch()` deja
    de reintentar contra ese agente, con una línea de log propia
    explicando el motivo (no la genérica "canal caído", que implica una
    recuperación que acá nunca llega). Solo una credencial nueva
    (`SetConfig`, vía `set_capi_connector`/`register_agent`) lo revive.
  - Un código que esta versión no reconoce (un CleverCoder viejo con el
    `terminal_not_listening` de antes, o uno nuevo del futuro) se trata
    como transitorio y queda en el log — nunca rompe ni descarta nada.
  - **Compatibilidad hacia atrás intacta**: contra un CleverCoder anterior
    a la 191 (sin el campo `error` en el body, o con un código que esta
    versión no conoce) el comportamiento es EXACTAMENTE el de antes de
    este cambio.
  - Gratis, mismo protocolo: el `chat_id` que arma CleverCoder ahora es
    estable y calculado; sin party, la forma sin agente nombra una
    POSICIÓN (el N-ésimo terminal del proyecto), no un terminal puntual —
    documentado en `store.Agent.AntennaTerminalID`, porque explica por qué
    `position_empty` es normal y no una credencial mal configurada.
  - `docs/T32-DIAGRAMA-MOTIVOS-NO-ENTREGA.md` con el detalle completo.
- **T14 — el buscador va dentro de las pestañas** (`ct-2026-08-05-1232`,
  el boss: *"y el buscador lo quiero dentro de las pestañas no fuera
  (antes)"*). El `<input id="search">` vivía suelto arriba de todo, antes
  del bloque de pestañas — buscando una sola cosa (conversaciones) pero
  posicionado como si buscara en todo, cuando en realidad solo actuaba
  sobre 3 de las 6 pestañas (Agentes/Reglas/Drafts nunca filtraron nada).
  - **Reposicionar, no duplicar.** El mismo `<input>` (uno solo, nunca
    copiado por pestaña) se movió a DESPUÉS de `.tabs-head` y ANTES de
    los `.tab-panel` — queda visualmente agrupado con el contenido de la
    pestaña activa, no separado arriba. `state.filterText`/
    `matchesNeedle`/`foldAccents` ya eran los únicos helpers que Chats,
    Grupos y Contactos compartían — no había lógica repetida que
    consolidar, solo un input mal ubicado prometiendo más de lo que hacía.
  - **`SEARCHABLE_TABS`** (`app.js`, nuevo) — el único lugar que decide
    qué pestañas buscan y con qué placeholder. El mismo handler que
    cambia de pestaña muestra/oculta el input entero para las que no
    filtran (Agentes/Reglas/Drafts) en vez de dejarlo presente y muerto.
  - **El filtro NO se arrastra al cambiar de pestaña** — se limpia
    (`state.filterText` + el valor del input) en cada cambio. Cada
    pestaña busca un universo distinto; una lista vacía sin ninguna pista
    de que hay un filtro viejo activo se lee como "está roto", no como
    "no hay resultados acá" — ahorrar un re-tipeo no compensa esa
    confusión.
  - Alternativa descartada: reparentar el `<input>` por JS dentro de cada
    `.tab-panel` en cada cambio de pestaña, en vez de dejarlo fijo entre
    `.tabs-head` y los paneles — más código por el mismo resultado
    visual, ya que las 3 pestañas que buscan comparten el mismo layout
    genérico.
  - `node --check` sobre `app.js`; smoke manual (binario compilado,
    `GET /dashboard/` verificado sirviendo el HTML/JS con la nueva
    estructura) — sin navegador disponible en este entorno para el pase
    visual completo.

### Verified
- **T17 Parte 2** (`ct-2026-08-05-1240`) — investigado antes de tocar
  código: el push name YA se guardaba (`TouchChat` en cada mensaje
  entrante) y la precedencia agenda → push name → número YA existía en
  el frontend (`app.js#renderRow`, desde S1h). No era un bug — cerraba
  solo un hueco de confirmación a nivel API
  (`TestChatsEndpointNameCarriesPushNameWithNoAgendaEntry`). Los números
  pelados que vio el boss son chats sin ningún mensaje entrante real —ahí
  no hay push name que mostrar—, y T18/T18B (mismo día) ya los saca de la
  pestaña Chats.

### Removed
- **T18B** (`ct-2026-08-05-1243`, sub-cambio sobre el hallazgo de T18,
  decisión del boss vía Citrino) — se retira `chat_groups` entera:
  `store.AddGroupMember`/`RemoveGroupMember` y la tabla misma. Nunca tuvo
  escritor en código de producción (solo tests) — dos tablas para la
  misma relación con solo una viva es la deuda que veníamos pagando todo
  el día; `group_members` (la que el sync real de grupos escribe) queda
  como fuente única.
  - `ChatOrigin`/`GroupsOf`/`get_chat_groups` (MCP) leen `group_members`
    ahora — `group_discovered` dispara de verdad por primera vez, y
    `get_chat_groups` devuelve datos reales en vez de vacío siempre.
  - La implementación paralela de `handleChats` (`isGroupMemberCanon`,
    dejada sin tocar en T18 por estar bloqueada en esto) se consolidó
    sobre `store.ChatOrigin` — cierra el punto 4 pendiente de T18.
  - **Verificado antes de borrar, no dado por hecho**: sin datos que
    migrar (confirmado, cero escritor real); `group_members` cubre
    grupos donde el gateway no es admin (verificado contra la librería
    whatsmeow vendorizada — sin filtro por rol en ningún lado);
    `group_members` solo se refresca al conectar/reconectar o con un
    `KickResync` explícito, nunca en vivo por evento — documentado como
    ventana de frescura conocida, no resuelto (no era lo pedido), no
    peor que antes (que nunca funcionaba en absoluto).
  - `resetTables` de 8 a 7 nombres. `reconcile.go`'s nota de alcance
    (fusión @lid↔número) actualizada a `group_members`.
  - `docs/T18B-DIAGRAMA-RETIRAR-CHAT-GROUPS.md` con el detalle completo;
    `docs/T18-DIAGRAMA-SEPARAR-POR-ORIGEN.md` marcado resuelto sin
    reescribirse.

---

## 0.1.6 — 2026-08-07

### Added
- **Preámbulo en todo despacho** (`ct-2026-08-05-0155`, boss verbatim: *"si soy
  boss tiene que decir is boss, y si no, el preámbulo son las reglas. Todo
  mensaje con su preámbulo"*). `dispatchPayload` tenía un `if !isBoss` que
  omitía el bloque entero justo para el dueño: su mensaje llegaba con el texto
  y el nonce, sin reglas y sin decir con quién se hablaba — el agente tenía que
  deducirlo. Comprobado en vivo. Ahora la línea de identidad va siempre
  (`is_boss` / `is_approver` / nivel) y las reglas efectivas se adjuntan para
  cualquier nivel, incluido el dueño, cuyas reglas propias se descartaban.
  `get_instructions` expone los mismos dos campos para el agente que se
  reconecta a mitad de camino.
- **Versión única de verdad** (`VERSION` en la raíz). Convivían tres números y
  ninguno coincidía: el código decía `0.1.0`, el instalador `0.1.4`, y lo
  publicado era `0.1.5`. `build-all.sh` inyecta por ldflags y genera el define
  del instalador; `server.go` usa la constante; **`get_status` expone el campo
  `version`** — es la única tool que responde sin despacho activo, así que un
  cliente puede preguntar contra qué versión habla apenas se conecta. `go:embed`
  no lee fuera del paquete y los symlinks no son portables en Windows:
  `internal/version` guarda una copia que el build resincroniza, con test que
  falla si divergen.

### Security
- **Datos personales fuera del repositorio.** Auditando antes del primer push
  público apareció `docs/S13-C1-PARES.csv` con **559 personas — nombre y
  teléfono**, más el JID del boss, nombres de contactos en comentarios y un
  grupo real en tests. Evidencia sacada del repo, ~124 identificadores
  sustituidos por prefijo `555` en 35 archivos, docs reescritos. Freno duro en
  `.githooks/pre-commit` (activar con `git config core.hooksPath .githooks`),
  reglas en `constitution.md §2b`. Boss verbatim: *"no deberia haber ni siquiera
  numeros de prueba… el server debe venir limpio, igual que un server nomral"*.
  Las cabeceras de copyright conservan la autoría: ahí el nombre es del autor.

### Docs
- **Manuales embebidos**: dónde se instala Piumy con rutas concretas, de dónde
  se descarga, la configuración MCP por defecto, y que **conectado no es
  habilitado** — sin despacho activo toda tool de acción responde `default DENY`,
  `get_status` es la puerta permitida, y un despacho no se fabrica.

### Published
- **Código fuente público**: `github.com/chamilonster/piumy-gateway`, AGPL v3,
  historia limpia (el historial de desarrollo contiene datos reales de pruebas).
  Repo separado del sitio a propósito: `piumy.app` se publica desde `master` de
  `chamilonster/Piumy` y un push encima lo habría tumbado.
- **Instaladores 0.1.5 y 0.1.6** como releases en `chamilonster/Piumy`.

### Known issue
- **El instalador no cierra el Piumy corriendo** (`ct-2026-08-07-0415-t34`, en
  vuelo). `AppMutex` detecta la instancia viva pero falta `CloseApplications`:
  muestra un diálogo pidiéndole al usuario que lo cierre a mano — y Piumy es app
  de bandeja, sin ventana. Peor: en modo desatendido **devuelve código 0 sin
  instalar nada**. Afecta a 0.1.5 y 0.1.6. Sale arreglado en 0.1.7.

---

## Histórico sin versión asignada

Entradas anteriores a que el proyecto empezara a publicar releases. Salieron
en alguna versión entre 0.1.1 y 0.1.6, pero fijar cuál exactamente requeriría
el commit-corte de cada una — no adivinado, queda sin asignar.

### Changed
- **T31** (`ct-2026-08-06-0244`, decisión del boss, revierte S10 —
  `ct-2026-07-30-1349` — para un solo botón) — `set_chat_rules` se
  desbloquea por MCP, **sin condiciones**. Reemplaza dos versiones de este
  contrato que el boss rechazó (una llave — despacho del chat del dueño —,
  después dos — esa más ser el agente principal). Su argumento, verbatim:
  *"No pongas condiciones, que la skill recomiende nada mas, me cargan que
  metan tantas limitaciones y frenos miedosos... ya es responsabilidad del
  usuario."* Respaldado con la evidencia del propio día: el whitelist que
  lo bloqueaba a él mismo (T30), las reglas vacías que dejaban el sistema
  mudo (T5), el cifrado obligatorio (T28) — frenos agregados por
  precaución, los tres deshechos después.
  - El handler llama `store.SetChatRules` directo — sin chequeo de nivel,
    sin `chatScopedArg` (cualquier `chat_id`, no solo el del despacho
    propio), sin requerir un despacho atado. Ausente de `bossOnlyTools`,
    `chatScopedArg` y `selfGatedTools` — cero gate, en ningún lado.
  - `set_type_rules`/`set_default_rules`/`set_is_boss` **sin cambios** —
    siguen MCP-BLOCKED incondicional, exactamente como S10 los dejó. El
    boss pidió destrabar las reglas de un chat, no las de alcance amplio.
  - La arquitectura que el boss sí quiere (agentes separados: uno con esta
    capacidad, otro que atienda desconocidos sin tenerla) queda como
    **recomendación** en la skill `piumy-operator` — tono de consejo de
    diseño, no de gate ni de advertencia de seguridad.
  - `docs/T31-DIAGRAMA-DESBLOQUEAR-SET-CHAT-RULES.md` con el detalle
    completo; `docs/S10-DIAGRAMA-GATE-DURO-CONFIG.md` queda intacto con
    una nota apuntando acá (misma disciplina que T28 con los docs de T2).

### Added
- **T16** (`ct-2026-08-05-123257`, pestaña Drafts — depende de T15) — el
  footer fijo "Confirmaciones pendientes" pasa a ser la 6ª pestaña del
  tablero, con el contador **sobre el botón** (`#draftbadge`, visible sin
  entrar a la pestaña, pedido literal del boss vía Citrino). Adentro:
  leer el mensaje, editar (`✎`), rechazar con motivo obligatorio (`↩`,
  modal propio, sin `window.prompt`), aprobar (`✓`) y descartar (`✕`) —
  los dos últimos ya existían, `editar`/`rechazar` llaman el backend de
  T15 (`edit-draft`/`reject-draft`). Una fila muestra "— ronda N" cuando
  el draft es un redraft (T15's `round`, ya viajaba en el JSON, nadie lo
  mostraba).
  - **El contador se actualiza solo** — pedido explícito de Citrino: "un
    borrador que aparece y no se ve hasta recargar es una respuesta que
    salió tarde." Nuevo `Event.Type` `"draft"` (`eventbus`), publicado
    desde los 6 puntos donde un draft se crea o se resuelve
    (`mcpserver/send.go`+`admin_tools.go`, `restapi/admin.go`) vía
    `publishDraftChanged` (un helper por paquete, no comparten
    internals). `app.js` cuelga `REFRESH_ON.draft` del mismo ciclo de
    auto-refresco SSE que ya existía (`docs/DASHBOARD-AUTO-REFRESH-2026-07-24.md`)
    — sin polling nuevo, el de 15s queda como red de seguridad.
    `mcpserver.Deps` gana `Bus *eventbus.Bus` (nuevo — `restapi.Deps` ya
    lo tenía) para que un draft resuelto por MCP (el boss diciendo
    "aprobá los pendientes") nudgee al tablero igual que uno resuelto
    desde la propia UI.
  - Verificado contra un binario real en scratchpad (DB sembrada a mano,
    nunca la instalación real): HTML/JS servidos contienen los elementos
    nuevos, `node --check` sobre el `app.js` servido, `edit`/`reject`
    (ronda normal y en el tope)/`approve`/`discard` mutan la lista como
    se espera, y una llamada real a `discard-draft` hizo aparecer
    `{"type":"draft"}` en el stream SSE (`curl -N /api/events`) —
    confirma el camino completo. Sin extensión de Chrome disponible en
    esta sesión para el click-through visual, dicho explícitamente.
  - `docs/T16-DIAGRAMA-PESTANA-DRAFTS.md` con el flujo completo.
- **T15** (`ct-2026-08-05-123241`, backend de borradores) — rechazar un
  borrador con motivo, tope de tres rondas, y editar sin aprobar.
  - `reject_draft`/`POST /api/admin/reject-draft` — a diferencia de
    `discard_draft` (final), pide otro intento: la razón queda grabada EN
    el draft (`drafts.reject_reason`, no un canal aparte — pedido explícito
    de Citrino: "el motivo tiene que viajar con el mensaje, no aparte") y,
    si la ronda rechazada es menor a `store.MaxDraftRounds` (3), el mensaje
    que la disparó vuelve a `PendingDedicated` (`store.MarkPendingBefore`,
    inverso de `MarkHandledBefore`) para que el próximo sweep de `capipush`
    redespache. `capipush.dispatchPayload` antepone el motivo + el borrador
    anterior al payload — el agente lo ve junto con el mensaje original, no
    tiene que ir a buscarlo.
  - Tope de rondas (`drafts.round`, calculado por `nextDraftRound` en cada
    `AddDraftWithConfirmer`): un redraft que responde a un rechazo continúa
    la cadena (ronda+1); cualquier otro caso (aprobado, descartado, chat sin
    drafts previos) arranca en ronda 1. Rechazar la ronda 3 registra el
    motivo pero NO redespacha — el ciclo automático para ahí, el dueño
    resuelve con `edit_draft`/`discard_draft`.
  - `edit_draft`/`POST /api/admin/edit-draft` — reemplaza el texto de un
    draft pendiente sin aprobarlo ni cambiar su status.
  - Ambas tools MCP, misma familia que `discard_draft`: nunca envían →
    restringen → siempre permitidas, ningún nivel de dispatch requerido
    (`selfGatedTools`, sin chequeo propio en el handler).
  - `docs/MANUAL.md`/`AGENT-BEHAVIOR.md` no tocado en el segundo (T15 no es
    parte del gate duro de envío) — solo `MANUAL.md`, con el detalle de
    arriba en las secciones `store`/`mcpserver`/`capipush`/`restapi`.

### Fixed
- **T30** (`ct-2026-08-06-0159`, Citrino, decisión del boss: *"el criterio
  de salida tiene que alinearse con el de entrada"*) — el chat del dueño
  (`is_boss=1`) queda exento del whitelist anti-ban del router, en las dos
  direcciones. Origen: Citrino recibió el mensaje de vuelta del boss y el
  gateway rechazó el envío por whitelist vacía — el chat tenía rules,
  había pasado el ritual entero, y aun así no se pudo contestar. Era el
  único de los cuatro gates de is_boss que no lo eximía:
  `initiateAuthorized` (enviar sin dispatch atado), `store.PendingDedicated`
  (T5, entrada a la cola) y `capipush.dispatch` ("is_boss ⟹ principal, sin
  configuración de router.json") ya lo hacían.
  - `mcpserver.validateSend` (`send.go`) — salta `d.Router.Resolve(to).Allowed`
    cuando `c.IsBoss`. Todo lo demás (muted, JID, claim, rules,
    grupo-no-ignorado, policy_version) se sigue aplicando igual.
  - **Hallazgo de yapa, recorriendo el camino completo — más grave que el
    reportado:** `corepipeline.handleInbound` (la entrada, no la salida)
    aplicaba el mismo whitelist sin excepción — un `router.json` sin el
    número del dueño descartaba sus propios mensajes ENTRANTES en
    silencio: sin guardar, sin publicar al eventbus, sin log. Peor que la
    salida, que al menos devuelve un error de tool visible. Nueva función
    `isBossChat(jid)` (`pipeline.go`) — mismo criterio, mismo "sin
    beneficio de la duda" que `initiateAuthorized` si el chat nunca se
    tocó.
  - El governor (`internal/whatsmeow`, el anti-ban de pacing/rate-limit
    real) — sin tocar, a propósito.
  - `AGENT-BEHAVIOR.md` (check 6 del gate duro) y `docs/MANUAL.md`
    actualizados — la excepción quedó escrita como decisión tomada
    (mismo criterio que T28), no como propiedad categórica que el
    próximo reconstruye. Documentado también el falso amigo: `chat.Status`
    usa los mismos literales `"whitelist"`/`"blacklist"` para un concepto
    de UI sin relación con el whitelist del router.
  - Tests nuevos: `TestSendMessageWhitelistBypassedForBossChat`
    (`mcpserver`) y `TestHandleInboundBypassesRouterGateForBossChat`
    (`corepipeline`) — el test existente que aserta el bloqueo para un
    chat NO-boss (`TestSendMessageWhitelistGateStillApplies`) queda
    intacto, su jid nunca tuvo `is_boss`.

### Removed
- **T28** (`ct-2026-08-05-2242`, decisión del boss, revierte T2 —
  `ct-2026-08-05-0205`) — el despacho ya NO lleva una segunda capa de
  cifrado propia. Boss verbatim: *"Clever coder es mío, y lo programo
  yo... Lo único que hace es buscar actualizaciones del mismo programa.
  Entonces, no hay nada de qué protegerse."* El canal cAPI (CleverCoder)
  ya es un túnel cifrado por handshake — la capa que T2 agregó adentro
  solo protegía el contenido de CleverCoder mismo, y CleverCoder es del
  dueño, en su propia máquina. **Sin flag, sin interruptor** (corrección
  sobre T27, que había propuesto dejarlo apagado por default: un flag
  apagado es justo el mecanismo por el que esto vuelve — alguien lo
  prende "porque estaba ahí"). Es la tercera vez que el boss pide esto —
  eso cambió el problema: el tramo incluyó auditar POR QUÉ volvía.
  - **Borrado, no deshabilitado:** `internal/capi` (Producer/Encrypt/Decrypt,
    AES-256-GCM) y `cmd/agentclient` (decrypt_dispatch) — paquetes
    enteros. `config.CAPIKey`/`PIUMY_CAPI_KEY`,
    `config.CAPIPlaintext`/`PIUMY_CAPI_PLAINTEXT`,
    `capipush.Config.Plaintext`, `restapi.Deps.Plaintext`/`CAPIProducer`,
    `agentconnect.Info/Params.CAPIKey`/`AgentClientPath` — ninguno
    sobrevive como campo vacío, todos dejaron de existir.
    `capipush.plaintextPayload` renombrada a `dispatchPayload` (ya no hay
    un segundo modo del que distinguirse). El instalador ya no genera ni
    preserva una 4ta clave ni empaqueta `agentclient.exe` — versión
    0.1.3 → 0.1.4. `build-all.sh` ya no compila el binario del agente.
  - **La causa real de que volviera tres veces:** cuatro lugares
    afirmaban el cifrado como propiedad estructural, en prosa categórica
    ("el cifrado nunca es opcional", "no es opcional") — el manual de
    conexión (`piumy-connect`), el del orquestador (`operacion.md`), el
    mapa del proyecto (`docs/MANUAL.md`) y un comentario del instalador.
    Los cuatro reescritos para decir que la decisión SE TOMÓ, no para
    describirla como regla del sistema — el próximo que los lea no tiene
    nada que "arreglar" ahí.
  - **Agregado del boss, mismo tramo:** los tres manuales de Piumy
    (`connect`/`operator`/`orchestrator`) enlazan a la skill
    `capi-protocol` (CleverCoder) para el protocolo real (handshake,
    pinpass, el túnel cifrado) en vez de redescribirlo — una sola fuente,
    el protocolo no es nuestro.
  - **Qué NO se tocó:** el AES-256-GCM propio de `CleverInjector`
    (`postMessage`/`deriveKey`) — es el túnel real de cAPI, el que
    justifica la decisión. `capiconn` (conectarse a la antena, concepto
    distinto). El cifrado de `sessionbackup`/`PIUMY_BACKUP_KEY` — feature
    aparte. Los docs históricos de diseño (F4/F4B/F5/S1/S4C/S5,
    `T21-DIAGRAMA-INSTALADOR-KEY-SAFETY.md`) — quedan como registro de la
    decisión ORIGINAL, no se reescriben (misma razón que la corrección de
    T25: la traza de qué se decidió vale más que un archivo que la
    borra). `.claude/skills/piumy-connect/SKILL.md` (copia local, no
    trackeada) queda con el texto viejo hasta que CleverCoder la
    resincronice desde la fuente ya corregida.
  - Detalle completo, con diagrama, en
    `docs/T28-DIAGRAMA-CAPI-SIN-CIFRADO.md`.

### Added
- **T29** (`ct-2026-08-06-0140`, Citrino) — manual de conexión
  (`piumy-connect`) y del orquestador (`operacion.md`) advierten cómo
  armar un `curl` cuyo cuerpo lleve texto humano: nunca inline en la línea
  de comandos, siempre por archivo UTF-8 + `--data-binary @archivo`. Boss
  verbatim: *"eso del armado debería ser automático o expresado con skill
  para que no falle en ningún idioma."* Origen: Citrino armó un JSON
  pasando texto por la línea de comandos y le llegó al boss con acentos
  rotos; mandado desde archivo llegó perfecto. No es un bug de
  piumy-gateway — `encoding/json`/`net/http` manejan UTF-8 bien siempre —
  la corrupción pasa antes, en la terminal del que llama: recodifica el
  argumento con su codepage local (casi nunca UTF-8) antes de que el
  proceso lo reciba. No es específico del español: en portugués o alemán
  se rompe una tilde, en chino o árabe el texto entero se pierde
  (reemplazado por `?`). Probado con las dos formas, lado a lado, en
  español/portugués/alemán + chino + árabe.
- **T25, hallazgo 2** (`ct-2026-08-05-1833`, decisión de Citrino tras el
  smoke) — `PIUMY_DEFAULT_TERMINAL_ID` vacío ya no deja a is_boss sin
  destino cuando la antena principal está configurada: el propio log de
  arranque mostraba la advertencia de "vacío" seguida, en la línea
  siguiente, del terminal real al que capipush ya despachaba todo lo
  demás — un cable de medio camino, no una configuración faltante.
  `resolveDefaultTerminalID` (`main.go`, función pura, testeada) usa ese
  mismo `terminalID` (KV `capi_terminal_id` o env) como respaldo — la
  variable de entorno sigue ganando si está puesta, y el WARNING real
  ahora solo dispara cuando NI el env NI la antena dan un terminal. Para
  ese último caso, alarma nueva en el tablero
  (`default_terminal_configured`, `GET /api/status` — mismo patrón que
  `factory_password`/`antenna_configured`, sin caché). Verificado en vivo
  (binario real, no la instalación del boss): configurar la antena
  MIENTRAS el proceso corre no aplica el respaldo solo (`PortFallback` es
  inmutable post-arranque, ya documentado en `capipush.go` — no es un
  hueco nuevo); reiniciar con la antena ya puesta sí lo aplica, log y
  alarma lo confirman. De paso: **corrección sobre el hallazgo 3** de esta
  misma investigación — no era un falso positivo. Citrino comparó el hash
  guardado del boss contra bcrypt de "piumy" de forma directa: da
  verdadero, T9 funcionó exactamente como debía. La prueba de login que
  parecía contradecirlo tenía un campo mal armado (`user` en vez de
  `username`). El test agregado en su momento
  (`TestFactoryPasswordAlarmIgnoresEnvSeedWhenOwnerAlreadyChangedIt`) sigue
  siendo válido, cubre un caso real, pero no era el causante. Detalle
  completo en `docs/T25-DIAGRAMA-SMOKE-WHATSAPP-SILENCIO.md`.
- **T25** (`ct-2026-08-05-1833`, primer smoke real sobre la instalación del
  boss) — WhatsApp no arrancó tras la actualización y no había NI UNA línea
  de whatsmeow en el log de arranque, ni éxito ni error. Descartadas con
  test aislado (no sobre la instalación real): el wiring
  `piumy-config.json`→env→`cfg.WADBPath` (sano), una ruta con espacio en el
  nombre (sana), un cambio de versión de la librería whatsmeow entre 0.1.1
  y master (idéntica). El hueco real que SÍ encontré: `Start()`/`New()`
  (`internal/whatsmeow/adapter.go`) nunca logueaban nada en el camino
  "sesión existente encontrada → Connect()" — indistinguible de "no se
  llamó" (el propio diagnóstico de Citrino). Dos `log.Printf` nuevos cierran
  la ambigüedad para el próximo smoke: `New()` dice si encontró una sesión
  previa (con el jid), `Start()` dice que va a conectar antes de intentarlo.
  Tests: `TestNewLogsNoExistingSession`/`TestNewLogsExistingSessionFound`.
  Hallazgo 3 (alarma de contraseña de fábrica en falso positivo): descartada
  la sospecha específica de Citrino (env `PIUMY_DASHBOARD_PASSWORD` de una
  instalación silenciosa interfiriendo) con test —
  `TestFactoryPasswordAlarmIgnoresEnvSeedWhenOwnerAlreadyChangedIt` prueba
  que la alarma y el login usan la MISMA comparación (`passHash`), no
  pueden discrepar por esta vía. Hallazgo 2 (`PIUMY_DEFAULT_TERMINAL_ID`
  vacío): NO codeado, dos propuestas a discutir con Citrino antes de tocar
  código (alarma en el tablero vs. auto-siembra desde el primer contacto,
  mismo patrón que T12). Detalle completo en
  `docs/T25-DIAGRAMA-SMOKE-WHATSAPP-SILENCIO.md`.
- S1a — dashboard: estilo terminal piumy.app + carita viva (`ct-2026-07-19-1517`,
  padre `ct-2026-07-19-1511` "release Piumy v1"): `internal/dashboard/web/` con
  la paleta del mockup aprobado, layout hero+cards, carita conectada al mood
  real (`GET /api/status` ahora expone `mood`/`queue`, ya existían en
  `state.Status`), buscador de conversaciones y tabla responsive en mobile.
  Cero endpoint de negocio tocado, cero funcionalidad existente rota.
- S1c — dashboard: antena por UI, pegar la línea completa (`ct-2026-07-19-1556`,
  padre `ct-2026-07-19-1511`): `POST /api/admin/capi-connector-line {line}`
  (`internal/restapi/admin.go`) parsea el string tal cual lo imprime
  `capi_credentials` y re-cablea en un paso — reusa `capiconn.ParseConnectorString`
  (factorizado de `internal/mcpserver` a `internal/capiconn` para no duplicar el
  parseo entre la tool MCP `set_capi_connector` y este endpoint) +
  `store.SetCAPIConnector`, mismo write path que el endpoint estructurado
  existente. Fuerza `http://127.0.0.1:<puerto>` siempre (la IP de LAN del string
  se descarta). Modal "Antena ⚡" del dashboard reescrito para calzar el mockup:
  un textarea + botón "Conectar", reemplazando el form de 3 campos sueltos +
  "Probar handshake" anterior.
- S1d — dashboard: auth (login admin/piumy + sesión + cambiar contraseña)
  (`ct-2026-07-19-1616`, padre `ct-2026-07-19-1511`) — SEGURIDAD. Archivo nuevo
  `internal/restapi/auth.go`: `POST /api/auth/login {username, password}`
  (usuario fijo `admin`, bcrypt contra `store.SettingDashPassHash` — se siembra
  con `"piumy"` si no hay hash guardado; mismo error para user/pass malos, sin
  enumeración) + cookie de sesión firmada HMAC (HttpOnly, SameSite=Strict) +
  `POST /api/admin/password` (valida la pass actual, guarda la nueva, rota el
  secreto de firma — invalida toda sesión existente, forzando re-login).
  `restapi.auth()` ahora acepta X-API-Key (sin cambios, sigue andando para
  MCP/curl) O una sesión válida — `APIKey==""` sigue 100% abierto, intacto
  (no se rompe el acceso programático que ya usaba Citrino). `/dashboard/`
  pasa a ser público (el shell no tiene secretos; es su JS el que muestra el
  login). `reset_dashboard_password` (MCP) también rota el secreto de sesión
  ahora — un reset de emergencia cierra sesiones de navegador existentes.
  Modal Config (cambiar contraseña) reintroducido, sacado a propósito en S1a.
  Verificado con playwright: login → dashboard → cambiar pass → sesión muere →
  pass vieja falla → pass nueva entra.
- S1e-1 — dashboard: recuperación de contraseña por WhatsApp (`ct-2026-07-19-1652`,
  padre `ct-2026-07-19-1511`) — SOLO WhatsApp, la vía correo es S1e-2 aparte.
  Archivo nuevo `internal/restapi/recover.go`: `POST /api/auth/recover
  {method:"whatsapp"}` genera un código de 6 dígitos (crypto/rand, hasheado con
  bcrypt, en memoria del proceso con TTL de 10 min — nunca persistido en claro)
  y lo encola (`store.Enqueue`, la MISMA cola que `send_message` — respeta
  governor/kill-switch/pacing, cero atajo al anti-ban) al self number
  (`state.OwnJID`, pelado del sufijo `:device` de whatsmeow con el helper nuevo
  `bareJID`) + a todos los chats `is_boss` (`store.BossJIDs`, método nuevo).
  Responde SIEMPRE el mismo mensaje, pase lo que pase adentro — sin estado que
  filtrar. Cooldown: un código activo por vez. `POST /api/auth/recover/verify
  {code, new_password}` valida (no vencido, no usado, bajo el tope de 10
  intentos — se quema si se excede) y resetea vía el mismo camino de S1d
  (`SetDashPassHash` + `RotateDashSessionSecret`, cierra toda sesión). Frontend:
  link "Recuperar por WhatsApp" en el login + overlay nuevo `#recovermodal`
  (código + nueva/repetir), cero CSS nuevo (todo ya portado en S1a/S1d).
  Verificado con playwright contra un harness descartable con una ruta
  debug-only (nunca en producción) que simula la entrega leyendo el outbox.
- S1e-2 — dashboard: recuperación de contraseña por correo/SMTP
  (`ct-2026-07-19-1716`, padre `ct-2026-07-19-1511`) — cierra S1e (recuperación
  completa). Reusa TODO el flujo de S1e-1 (código, hash en memoria, TTL, tope
  de intentos, cooldown, verify, rotar secreto); solo agrega el canal.
  `Deps.SMTP` (env `PIUMY_SMTP_HOST/PORT/USER/PASS/FROM`, default puerto 587
  STARTTLS) + `Deps.SMTPSend` (seam mockeable en tests, cae a
  `net/smtp.SendMail` real). `tryStartRecovery` refactorizado a
  `deliver(code)` callback — `handleRecover` ahora switchea
  `method:"whatsapp"|"email"`, cooldown compartido entre canales (el código
  en memoria es solo el hash, no hay plaintext que reenviar por el otro
  canal). `GET/POST /api/admin/recovery-email` — nuevo KV
  `dashboard_recovery_email`, validación liviana (solo `@` + sin espacios).
  `handleRecoverVerify` sin cambios (no le importa qué canal entregó el
  código). Frontend: overlay de recover ahora elige vía (WhatsApp/correo, dos
  botones) en vez de auto-enviar por WhatsApp; campo de correo en el modal
  Config. Cero CSS nuevo. Verificado con playwright contra un harness
  descartable con SMTP mockeado.
- S1f — dashboard: el panel admin se activa recién tras vincular WhatsApp
  (`ct-2026-07-19-1735`, padre `ct-2026-07-19-1511`) — casi todo frontend,
  reusa `GET /api/status` (`connected`/`show_qr`), `GET /api/qr/image` y el
  overlay de QR existente. Las 3 cards del panel admin se envolvieron en
  `#adminpanel` (oculto por default); `applyLinkGate` (`app.js`) lo
  muestra/oculta según `s.connected`, y auto-abre/cierra `#qroverlay` en la
  dirección contraria — cambia CUÁNDO se muestra el overlay, no cómo. Carita
  chica estática agregada al overlay de QR (mismo patrón `.login-head` de
  S1d). Transición en vivo vía SSE: `clearErrorState`/`handleDisconnect`
  (`internal/whatsmeow/inbound.go`) publican `wa_connected`/
  `wa_disconnected` — `wa_connected` es evento nuevo, sin él la transición
  hubiera tardado hasta 15s (el poll) en vez de ser instantánea.
  Correcciones de backend mínimas encontradas auditando el contrato:
  `MOOD_FACES` no tenía entrada `"qr"` (ni la tuvo nunca el `KAOMOJI_CATALOG`
  original de Piumy — ahí es un path de render sin cara) y `main.go` nunca
  seteaba `Mood="qr"` al emitir un QR — ambas agregadas, con el reset
  correspondiente en `clearErrorState` para no quedar pegado en "qr" tras el
  primer vínculo. Cero CSS nuevo salvo una utilidad genérica `.hidden`.
  Verificado con playwright contra un harness descartable arrancando sin
  vincular, con 2 rutas debug-only que simulan escanear/desconectar.
- S1g — dashboard: organizar la lista, 1:1 arriba + grupos colapsables al
  fondo (`ct-2026-07-19-1801`, padre `ct-2026-07-19-1511`) — pedido fresco
  del boss (dictado confuso, lectura de Citrino marcada como corregible).
  Resuelve que el backfill de contactos y el scraping de miembros de grupo
  (~717 en la escala real) no se mezclen con los 1:1 reales en una lista de
  600+. `GET /api/chats` ahora clasifica cada fila con `type:
  "p2p"|"group"|"group_member"` (+ `group_jid` en `group_member`): p2p
  exige un mensaje real guardado (`store.ChatJIDsWithMessages`, nuevo) o
  `is_boss` (carve-out agregado para no esconder ese flag crítico); un
  contacto sin mensaje y sin ser grupo/boss queda excluido del todo (ruido);
  cada `group_members` row se proyecta como `group_member` salvo que su
  número ya haya ganado como p2p ("gana el 1:1", contrato literal); un
  miembro en varios grupos aparece bajo cada uno. `dashboardChatLimit=5000`
  reemplaza el límite efectivo de 20 que traía `ListChats(0)` — con 600+
  chats reales ese límite hacía la clasificación casi inútil.
  `store.ListAllGroupMembers` (una sola query, no N+1). Frontend: la tabla
  1:1 existente queda intacta; grupos se renderizan aparte (`#groupzone`)
  con cabecera colapsable + contador, colapsado por default y persistido
  por grupo en localStorage; la búsqueda atraviesa grupos colapsados
  (auto-expande el que tenga un match). Verificado con playwright: 1:1
  arriba sin ruido, grupos colapsados con contador correcto, toggle
  persiste tras reload, búsqueda expande el grupo correcto.
- S1b — dashboard: cablear el estado real (governor/backup/cifrado)
  (`ct-2026-07-19-1823`, padre `ct-2026-07-19-1511`) — **CIERRA EL
  DASHBOARD**. Los badges que S1a dejó honestamente sin dato falso ahora
  llevan dato real. `GET /api/status` suma `antenna_configured` (¿hay un
  endpoint del connector guardado? — reemplaza el criterio viejo del badge
  Antena, `agents > 0`, que en realidad medía otra cosa),
  `governor_rate_per_min`/`governor_killed` (`Governor.Max()`/`Killed()`,
  ya existían), `backup_messages`/`backup_group_members`/`backup_contacts`
  (`store.BackupCounts`, nuevo — 3 `COUNT(*)` livianos sin caché) y
  `backup_encrypted` (`Deps.Backup.Enabled()` — reusa
  `sessionbackup.Backuper` en vez de duplicar el chequeo de
  `PIUMY_BACKUP_KEY` como un bool aparte). Cada campo nil-safe por su
  cuenta — sin esa dependencia wireada, ese campo solo cae a su zero
  value, nunca 503 el endpoint entero. Frontend: 3 badges nuevos en la
  `.status-bar` (Governor/Backup/Cifrado), cero CSS nuevo. Verificado con
  playwright: los 5 badges con dato real de una, más una ruta debug-only
  que dispara el kill switch para confirmar que "Governor" pasa a
  "⛔ kill" en vivo, sin recargar.
- S2a — e-paper: portar el renderer de la carita (`ct-2026-07-19-1843`,
  padre `ct-2026-07-19-1511`) — arranca la Parte 2 del release (módulo
  e-paper). RESCATE, no reescritura: `adapters/display/` nuevo (Python,
  autocontenido, no toca el binario Go) con copia byte-a-byte fiel de
  Piumy (`adapters/display/`) — `render.py` (`KAOMOJI_CATALOG` 19 moods +
  motor de gaze de 3 tipos de ojo + QR fullscreen), `backend.py` (factory
  `get_backend()`), `file/` (backend de archivo, dev/CI sin hardware),
  `fonts/` (DejaVuSans + Bold bundleadas) y `requirements.txt`
  (Pillow + qrcode[pil]). Verificación: `python render.py <outdir>`
  genera las 19 caritas + QR — comparado con un render idéntico corrido
  contra la fuente Piumy, `diff -q` no encontró ninguna diferencia entre
  los 35 PNG de ambos lados. Decisión propia: NO se portaron
  `file/render.py`/`file/faces.py`/`file/display.png`/
  `file/requirements.txt` — código huérfano de una versión anterior a que
  existiera el `render.py` compartido, no referenciado por
  `backend.py::get_backend()` ni por nada más en el módulo; portarlo
  hubiera resucitado un renderer duplicado y confuso sin ningún llamador
  real. `NOTES.md` nuevo documenta cómo correr el self-check. Pendiente
  para S2b/S2c: `service.py` (loop) y `epaper/` (driver Waveshare) —
  fuera de este subcontrato.
- S2b — e-paper: service loop + contrato status.json/face.json
  (`ct-2026-07-19-1853`, padre `ct-2026-07-19-1511`) — rescate de
  `service.py` (copia byte-a-byte fiel): polea `status.json` por mtime,
  decide refresh full vs. parcial (flash solo al entrar/salir de
  `qr`/`error`/`sleeping`, opt-in vía `PIMYWA_EPAPER_FULL_REFRESH`),
  cadencia de animación dinámica FAST→SLOW ("sobre de atención"), y
  escribe el sidecar `face.json` con `pick_variant`/`variant_repr` de
  `render.py` (S2a) — cero catálogo duplicado. Verificación del contrato
  con el gateway: `internal/state/state.go` **ya** escribe `mood` en
  `status.json` (campo sin `omitempty`, siempre presente) vía
  `PIUMY_STATUS_PATH`, y `state.ValidMoods` ya tiene los 19 moods exactos
  del catálogo — **cero cambio Go necesario**, el gate estaba cherry-pickeado
  desde el principio. Probado en PC con el backend de archivo: cambiar el
  mood a mano en un `status.json` de prueba actualiza `face.json` + el PNG
  con la cara correcta en el siguiente poll (verificado con mood `idle`
  → `vip`, `(♥o♥)` correcto). Dos decisiones propias (marcadas en
  `NOTES.md`): (1) `face.json` NO se cablea de vuelta al gateway —
  opcional per contrato, el dashboard ya mapea mood→kaomoji en JS (S1a),
  sin consumidor real hoy para el string exacto de variante; (2) el
  módulo mantiene el prefijo de env vars `PIMYWA_*` heredado de Piumy
  (vs. `PIUMY_*` del resto del gateway) — no chocan en runtime (procesos
  separados) pero hay que alinear `PIMYWA_STATUS`/`PIUMY_STATUS_PATH`
  explícitamente al desplegar; queda para que Citrino decida si
  estandarizar. `.gitignore` suma `__pycache__/`/`*.pyc`. Pendiente para
  S2c: `epaper/` (driver Waveshare).
- S1g-fix — dashboard: MOSTRAR los contactos no-boss (`ct-2026-07-19-1905`,
  padre `ct-2026-07-19-1511`) — feedback del boss en vivo: la lista solo
  mostraba `is_boss` + grupos, faltaban los números no-boss. `handleChats`
  (`internal/restapi/read.go`) ya no excluye contactos no-grupo sin
  mensaje/sin `is_boss` — todo chat no-grupo es `"p2p"` ahora, con o sin
  mensaje. Cero cambio en el frontend: el sort default `recientes` (por
  `last_ts` descendente) ya ordena los sin-mensaje al final, y `timeAgo(0)`
  ya mostraba "sin mensajes". El segundo ítem del mismo subcontrato
  (scroll horizontal de la tabla) queda **pausado a pedido de Citrino** —
  viene un rediseño mayor a 3 tabs que probablemente cambia esta tabla,
  se retoma con el subcontrato nuevo.
- S2c — e-paper: driver Waveshare 2.13" V4 + estandarizar env a `PIUMY_*`
  (`ct-2026-07-19-1919`, padre `ct-2026-07-19-1511`) — rescate de
  `epaper/backend.py` de Piumy: `EPaperWaveshareBackend`/
  `_PanelController`, driver `epd2in13_V4` a mano con `spidev`+`gpiod` v2
  (sin la lib `waveshare_epd`), misma política de refresco pwnagotchi-style
  del resto del módulo. Defensivo (el punto clave del subcontrato): los
  imports de hardware están dentro del `__init__` de `_PanelController`,
  nunca a nivel de módulo, y `_try_init()` los envuelve en `try/except` —
  sin `spidev`/`gpiod` degrada a no-op con `warning`, nunca crashea.
  Verificado en esta PC (Windows, sin esas libs instaladas):
  `get_backend("epaper-waveshare")` importa y degrada OK, sin excepción.
  El test con panel físico real queda para S3 (Pi Zero 2), aparte. De
  paso, estandaricé los env vars de todo el módulo (`backend.py`,
  `service.py`, `file/backend.py`, `epaper/backend.py`) de `PIMYWA_*`
  (heredado de Piumy) a `PIUMY_*` — lo que S2b había dejado marcado
  pendiente. El path de `status.json` se renombró literalmente a
  `PIUMY_STATUS_PATH` (antes `PIMYWA_STATUS`), igual que el env del
  gateway Go, así un solo `EnvironmentFile` alimenta a los dos procesos
  (los DEFAULTS siguen sin alinear — sigue haciendo falta setear la
  variable al desplegar). `NOTES.md` suma la tabla completa de env vars +
  pinout GPIO de la Pi Zero 2 + pasos de instalación (apt/pip/systemd).
- **0.1.2 — instalador: el canal del agente queda listo de fábrica**
  (padre `ct-2026-08-05-0155`, tramos T1-T4). Hasta 0.1.1, un despacho
  cAPI real llegaba cifrado y ningún agente podía leerlo — la clave que
  lo descifra vive deliberadamente solo del lado del agente
  (`cmd/agentclient`), pero nada en el instalador la generaba, compilaba
  ni publicaba.
  - **T1** (`ct-2026-08-05-015542`) — `internal/agentconnect`: el gateway
    escribe `agent-connect.json` al arrancar, junto a `status.json`
    (mismo data dir, derivado de la config) — `mcp_url`/`rest_url`/
    `mcp_key`/`rest_key`, para que un agente descubra cómo hablarle sin
    parsear `run-piumy.bat`.
  - **T2** (`ct-2026-08-05-0205`) — `installer/windows/piumy.iss` genera
    la 4ta clave (`PIUMY_CAPI_KEY`) y empaqueta `agentclient.exe`
    (compilado desde `cmd/agentclient`) al lado de `Piumy.exe`;
    `agent-connect.json` suma `capi_key`/`agentclient_path`. Cifrado
    intacto, nunca opcional.
  - **T3** (`ct-2026-08-05-0225`) — `get_manual(role:"connect")`: la
    skill que le dice a un agente, paso a paso, cómo conectarse
    (`internal/mcpserver/manuals/connect/SKILL.md`).
  - **T4** (`ct-2026-08-05-0256`, este) — Inno Setup instalado en la
    máquina de build (`winget install JRSoftware.InnoSetup`, no estaba —
    T2 tuvo que simular con el `.bat` a mano). Bump de versión a 0.1.2
    (el contenido del paquete cambió: 4ta clave + `agentclient.exe`).
    `build-all.sh` → `dist/` (6 targets del gateway + `agentclient-
    windows-amd64.exe`, nuevo desde T2) → `ISCC piumy.iss` →
    `dist/Piumy-Setup-0.1.2.exe` (21.483.614 bytes). Verificado que
    empaqueta `agentclient.exe` sin ejecutar el instalador: el log del
    propio compilador lista `Compressing:
    ...\agentclient-windows-amd64.exe` como parte de este build exacto,
    y el tamaño del instalador creció 4.010.503 bytes (~3,82 MiB) sobre
    el 0.1.1 (que no lo incluía) — consistente con ese binario de 8,67 MB
    comprimido lzma2. Intenté además una inspección de terceros
    independiente del log de compilación (7-Zip, `innoextract`) — ninguna
    lee el formato de Inno Setup 6.7.3 todavía (7-Zip no trae códec Inno;
    `innoextract` 1.9 solo llega a Inno 6.0.5), documentado para quien lo
    retome. **El instalador no se ejecutó** — el boss tiene 0.1.1
    corriendo con su sesión de WhatsApp pareada y su base real;
    reinstalar es decisión suya.
  - **T6** (`ct-2026-08-05-0315`, BUG bloqueante encontrado por Citrino
    en el instalador de T4 antes de que se llegara a distribuir) —
    `CurStepChanged/ssPostInstall` generaba las 4 claves **siempre**, sin
    mirar si ya había una instalación: reinstalar sobre una instalación
    existente rotaba `PIUMY_BACKUP_KEY` (backups cifrados viejos
    ilegibles para siempre) y `PIUMY_MCP_KEY`/`PIUMY_REST_KEY` (todo lo
    ya cableado contra las viejas dejaba de andar). Le pasaba a
    cualquiera que actualizara, no solo al boss (que tiene 3 backups
    reales cifrados con la clave actual). Fix: antes de generar, si
    `{app}\run-piumy.bat` ya existe, se lee (`LoadStringsFromFile`) y
    cada clave presente se reusa tal cual (`KeyOrGenerate` +
    `ExistingBatKey`, nuevas en `piumy.iss`) — solo se genera la que
    falta (el caso real de actualizar una 0.1.1: las 3 viejas se
    conservan, `PIUMY_CAPI_KEY` se crea porque no estaba).
    `ponytail:` documentado en el propio `.iss` — parsear el `.bat` es
    la única fuente que tiene una 0.1.1 (`agent-connect.json` no existía
    ahí todavía), camino de upgrade: mover las 4 claves a un archivo de
    config propio. La contraseña del tablero se investigó aparte —
    **ya estaba bien** (`passHash` en `internal/restapi/auth.go` es
    seed-only, confirmado con el test existente
    `TestPassHashIgnoresEnvSeedWhenHashAlreadyExists`), no se tocó nada
    ahí. Verificado con un instalador de prueba aislado (mismas
    funciones `[Code]` copiadas y diffeadas contra `piumy.iss`, corrido
    con `/DIR` sobre carpetas propias, nunca sobre la instalación del
    boss) en tres escenarios: instalación completa con 4 claves
    conocidas → las 4 salen intactas; launcher estilo 0.1.1 (3 claves,
    sin `PIUMY_CAPI_KEY`) → las 3 se conservan y la 4ta aparece nueva
    (64 caracteres hex); instalación limpia → las 4 nuevas, con los
    largos correctos (16/24/32/32 bytes). `Piumy-Setup-0.1.2.exe`
    recompilado con el fix adentro — la versión no se mueve, 0.1.2
    todavía no se había distribuido.
  - **T7** (`ct-2026-08-05-1125`) — instalador real (no un espejo) contra
    una carpeta de prueba propia, previo a publicar en el release público
    de `chamilonster/Piumy`. Primer intento: bloqueante real con
    `/VERYSILENT` — la página de la clave corre su validación igual
    (`Values[]` vacíos) y aborta sin escribir nada; resuelto por T8.
    Retomado después: instalación real con puertos propios preexportados
    (`PIUMY_MCP_ADDR`/`PIUMY_REST_ADDR`, heredados por el `Exec()` del
    instalador) → `netstat` confirmó `LISTENING` real, `agent-connect.json`
    con los 6 campos coincidiendo con `run-piumy.bat`. Sembré un mensaje
    boss directo en el `piumy.db` recién creado, relancé el mismo
    `Piumy.exe` instalado con las mismas claves + variables de captura de
    despacho → `capipush: despacho OK ... nonce=3622` real, descifrado con
    el `agentclient.exe` INSTALADO (no una copia) y la `capi_key` de su
    propio `agent-connect.json` — nonce coincidente, texto exacto. Cierra
    el circuito completo antes de publicar.
  - **T8** (`ct-2026-08-05-1133`) — el defecto real detrás de los dos
    síntomas: la página de la clave solo contemplaba "un humano
    instalando por primera vez". El boss lo vio en pantalla al
    reinstalar (le pedía una clave que `passHash()` —seed-only, T6—
    después descarta) y T7 lo encontró desde el otro lado (sin camino
    desatendido). Un solo arreglo para los dos:
    - **Instalación existente → la página no aparece.** `ShouldSkipPage`
      nuevo, misma detección de T6 (`run-piumy.bat` ya presente). No hay
      clave que pedir, la que hay se conserva.
    - **Modo silencioso → la clave entra por `/DASHBOARDPASSWORD=`**
      (`{param:...}`, mecanismo nativo de Inno) en vez de validar una
      página que nadie ve. `/RECOVERYEMAIL=` opcional, mismo criterio.
    - **Silencioso, instalación limpia, sin parámetro → NO aborta**
      (corrección del boss sobre el diseño original, verbatim: "que por
      defecto la clave del instalador sea piumy si se hace instalacion
      silenciosa"): cae al mismo default de fábrica que ya usa
      `auth.go` (`dashboardDefaultPassword = "piumy"`) — no inventa una
      clave nueva, reusa la que YA existía como fallback del lado del
      gateway. Lo deja bien visible en el log de instalación (`Log`, no
      un `MsgBox` que en silencioso nadie lee) para quien despliegue
      muchas máquinas desatendidas. Precedencia verificada: parámetro
      primero, `"piumy"` solo como último recurso, nunca al revés.
    - **Parámetro presente pero inválido (< 4 caracteres) → sí aborta**
      (`RaiseException`, no `MsgBox`) — ahí no es "no me dieron nada",
      es "me dieron algo mal". Verificado con un arnés de prueba aparte
      (mismas funciones, borrado): `RaiseException` da exit code 1
      distinguible de éxito, y el mensaje SÍ queda en el log aun con
      `/SUPPRESSMSGBOXES`.
    - Instalación interactiva limpia: **sin cambios** — la rama
      `WizardSilent` es nueva, la validación original queda intacta
      debajo, sin tocar.
    Verificado con el instalador REAL (`Piumy-Setup-0.1.2.exe`, no un
    espejo), 4 casos, en carpetas propias, nunca sobre la instalación
    del boss: (1) silencioso+limpio+sin parámetro → instala con
    `"piumy"`, log con la línea de aviso, `agent-connect.json` con los 6
    campos; (2) silencioso+limpio+parámetro válido → instala con esa
    clave, sin la línea de aviso; (3) silencioso+limpio+parámetro
    inválido → exit code 1, nada instalado; (4) silencioso+actualización
    (launcher estilo 0.1.1 + `secrets/` con un archivo real adentro) →
    página saltada (sin línea de aviso), las 3 claves viejas intactas,
    la 4ta nueva, el archivo de `secrets/` sin tocar. `Piumy-Setup-
    0.1.2.exe` recompilado con el fix — versión sin mover, seguía sin
    publicarse.
  - **T10** (`ct-2026-08-05-1203`, pedido directo del boss: "compruebe si
    funciona el instalador y el dashbord" — T7 probó el instalador y el
    gateway, nadie había mirado el tablero) — instalación real,
    silenciosa, en carpeta propia, dejada VIVA (no una corrida y
    apagado) para revisión externa. Verificado por HTTP desde este lado
    (sin navegador — esa herramienta la tiene Citrino/el boss): `GET
    /dashboard/` → 200 HTML real; login con clave incorrecta → 401;
    login con la correcta → 200 + cookie de sesión; `GET /api/status`
    sin autenticar → 401, autenticado → 200. **Hallazgo real, no de la
    prueba**: al revisar el tablero recién instalado, el boss usó el
    único CTA visible (el botón de QR) y vinculó su WhatsApp REAL a la
    instalación de prueba — detectado por el log (`whatsmeow: connected`,
    2 grupos reales) al recompilar para T9, nunca provocado a propósito.
    Bajada la instancia de inmediato, sin borrar `whatsmeow.db` (borrar
    el archivo NO desvincula el dispositivo del lado de WhatsApp — deja
    un dispositivo fantasma; el boss lo desvincula desde el teléfono).
    Contraorden del boss después ("es un telefono de pruebas... hay
    reglas"): la instancia se relanzó y queda VIVA a propósito —
    `POST /api/admin/kill {"kill":true}` activado y verificado en dos
    capas de código real (`corepipeline/outbox.go` + `whatsmeow/
    adapter.go`, no un flag decorativo), para servir de banco de pruebas
    con sesión/grupos/contactos reales sin poder mandar nada. **Hallazgo
    de producto anotado, no corregido acá**: nada en el dashboard avisa
    antes de vincular un WhatsApp real a una instalación de prueba.
  - **T9** (`ct-2026-08-05-1137`, boss verbatim: "si la contrase[ñ]a es
    piumy, mostrar un mensaje que diga, cambiar contraseña por defecto
    como alarma en el dashboard, y que lo lleve a la zona de opciones
    dode se cambia la contraseña") — T8 deja una instalación silenciosa
    sin parámetro en `piumy`, una clave pública (está en el instalador y
    en el código abierto). `isFactoryPassword(st)` (`auth.go`) reusa
    `passHash` (una sola fuente para el hash actual — dos formas de
    llegar al mismo default es cómo se desincronizan, criterio de
    Citrino en T8) + `bcrypt.CompareHashAndPassword` contra
    `dashboardDefaultPassword`, 100% server-side — nunca viaja una
    contraseña, solo un booleano nuevo (`factory_password`) en `GET
    /api/status`. Frontend: `#factorypwalert`, una barra FUERA de
    `#adminpanel` a propósito (visible aunque WhatsApp no esté
    vinculado — la ventana de mayor riesgo es la instalación recién
    hecha). `openConfigModal` factorizada del handler de `configbtn` —
    el botón de la alarma abre el MISMO modal de config existente,
    enfocado en la contraseña, sin pantalla nueva. Desaparece sola: el
    `toggle("hidden", ...)` corre en cada poll de `loadStatus` (15s) y
    en el re-login forzado que ya dispara un cambio de contraseña
    (`RotateDashSessionSecret` mata la sesión, `showLogin()` +
    `submitLogin()` vuelven a pedir el status) — sin lógica de
    desaparición aparte. Tests nuevos: factory=true en un store fresco,
    false tras cambiar la clave, false con `PIUMY_DASHBOARD_PASSWORD`
    seedeado (instalación silenciosa CON parámetro). Verificado además
    por HTTP contra una instancia real aislada (sin parear WhatsApp —
    la alarma no lo necesita): login con "piumy" → `factory_password:
    true`; cambio de clave → sesión vieja muerta, clave vieja
    rechazada, clave nueva acepta con `factory_password:false`.
- **T12** (`ct-2026-08-05-1231`, padre `ct-2026-08-05-0155`, boss verbatim:
  "un problema, el selfnumber no se auto define como boss automaticamente")
  — el número con el que se vincula WhatsApp ahora se marca dueño solo, sin
  que nadie toque el tablero. `whatsmeow.recordOwnIdentity` (ya existía,
  corre en cada `*events.Connected`) ahora también llama `markOwner(jid,
  name)`: `TouchChat` (garantiza la fila con los defaults de un chat
  individual normal — evita el default crudo `confirmation_mode='always'`
  de una fila insertada desde cero, que habría dejado cada respuesta al
  dueño esperando su propia aprobación) + `store.MarkOwnerIfUntouched(jid)`.
  Columna nueva `chats.is_boss_touched` (propia, independiente de
  `config_level_source`: rastrea SOLO si `is_boss` ya fue decidido, para
  que un cambio de `active`/`confirmation_mode` ajeno no lo cuente como
  "decidido") — `SetIsBoss` la marca en 1 en cada llamada (cualquier
  dirección), así que si el dueño se desmarca a mano, una reconexión
  posterior NUNCA se lo vuelve a poner. Migración atómica propia
  (`migrateIsBossTouched`, backfillea todas las filas preexistentes a 1 en
  la misma transacción — mismo patrón que `migrateConfirmationMode`): un
  `DEFAULT 1` plano en el `ALTER` habría envenenado también las filas
  NUEVAS (`TouchChat` nunca nombra la columna), dejando el auto-marcado
  permanentemente inerte. Ningún otro número/chat se toca — solo
  `state.OwnJID`, tal como pide el contrato ("no inventes heurísticas").
  Tests: instalación limpia marca dueño, reconexión tras desmarcado manual
  lo deja desmarcado, ningún otro chat cambia (`internal/store/chat_test.go`,
  `internal/whatsmeow/inbound_test.go`).
- **T21** (`ct-2026-08-05-1308`, CRITICAL, bloqueaba la publicación —
  revisión independiente de Amatista R2) — el instalador de Windows ya no
  puede pisar claves que no pudo leer con certeza. `ExistingBatKey`/
  `KeyOrGenerate` (T6) exigían un match exacto de `set VAR=`; si fallaba
  (el caso real: el `.bat` abierto en el Bloc de notas y guardado como
  "Unicode"), generaba una clave nueva en silencio y pisaba el launcher
  viejo — los backups cifrados con la clave anterior quedaban ilegibles
  para siempre. Regla fijada por Citrino: "ante la duda, no se pisa nada".
  - `ResolveLauncherKeys` reescrito: lee bytes crudos (`LoadStringFromFile`
    como `AnsiString`, nunca `String` — evita que una conversión Unicode
    implícita se coma la marca de orden de bytes antes de poder
    detectarla), aborta si detecta UTF-16 LE/BE o UTF-8+BOM, parser
    tolerante propio (`SET` mayúscula o minúscula, comillas, espacios
    alrededor del `=`, última coincidencia gana en vez de la primera).
    Exige las 3 claves base (MCP/REST/BACKUP); si falta cualquiera de esas
    tres, aborta sin completar lo que falta — CapiKey ausente es la única
    excepción legítima conocida (launcher 0.1.1) y ahí sí se genera.
  - **Rediseño, no parche, tras probarlo en vivo:** la primera versión
    llamaba `RaiseException` desde `ssPostInstall` — colgaba para siempre
    en cualquier aborto real (el log mostraba "Installation process
    succeeded" ANTES de la excepción: los archivos ya estaban copiados, y
    sin `/SUPPRESSMSGBOXES` funcionando de verdad — roto por un bug de
    mangling de paths de Git Bash en las pruebas — el diálogo de error
    interno de Inno esperaba un click que nunca llega). Fix real: la
    resolución de claves se movió a `PrepareToInstall` — el hook de Inno
    que corre ANTES de copiar un solo archivo; devolver un string no vacío
    ahí cancela la instalación entera, exit code distinto de cero, sin
    dejar nada a medio escribir.
  - `PIUMY_REST_ADDR` (agregado durante las pruebas, ver abajo): si el
    launcher anterior lo tenía a mano, se preserva en el nuevo — antes se
    perdía en silencio en cada reinstalación.
  - Secundarios del mismo archivo: desinstalación con default "No" en vez
    de "Sí" (`MB_DEFBUTTON2`); `AppMutex=PiumyGatewaySingleInstanceMutex`
    en `[Setup]`, creado por el binario mismo (`appmutex_windows.go`,
    `CreateMutexW`, apenas entra a `main()`) — sin esto, reinstalar/
    desinstalar con Piumy corriendo dejaba el ejecutable viejo corriendo
    emparejado con el launcher nuevo.
  - **Agregado en el camino** (Citrino, detectado al ver el efecto real
    durante las pruebas silenciosas de este mismo cambio: le abrían
    pestañas del tablero al boss): `LaunchFirstRunAndOpenDashboard` abría
    `http://127.0.0.1:8092/dashboard/` con el puerto escrito a mano — con
    otro `PIUMY_REST_ADDR` abría el tablero de otra instalación. Fix: usa
    el puerto real (preservado o default) y **no abre nada en modo
    silencioso** (`WizardSilent` corta el `ShellExecAsOriginalUser`; el
    `Exec` que arranca la app sigue igual).
  - Probado en vivo contra el instalador real compilado
    (`//VERYSILENT //SUPPRESSMSGBOXES`): UTF-16/truncado/bloqueado
    abortan con exit code distinto de cero y CERO archivos copiados;
    upgrade normal 0.1.1 (3 claves) y 0.1.2 (4 claves) intacto; duplicados
    resuelven a la última línea; puerto custom preservado y confirmado
    escuchando ahí; reinstalar/desinstalar con la app corriendo aborta
    (exit 1) sin tocar el binario. Detalle completo en
    `docs/T21-DIAGRAMA-INSTALADOR-KEY-SAFETY.md`.
  - `go build ./... && go vet ./... && go test ./...` verde. Instalador
    recompilado (`dist/Piumy-Setup-0.1.2.exe`, ISCC 6.7.3).
- **T20** (`ct-2026-08-05-1301`, revisión independiente de Amatista R1,
  sobre el merge de T5) — el contador de saturación de `capipush`
  (`store.CountRecentPendingNonBoss`) quedó ciego a los chats en modo
  `auto`. T5 (ct-2026-08-05-0311) ensanchó `PendingDedicated`/
  `CountPendingDedicated` a `mode IN ('dedicated','auto')` para que esos
  chats DESPACHEN, pero `CountRecentPendingNonBoss` — una TERCERA query
  con el mismo filtro, no nombrada en el pedido original de T5 — se quedó
  en `mode = 'dedicated'`. Efecto: una avalancha de chats `auto` nunca
  tripeaba el freno de backpressure, aunque sí seguía despachando sin
  frenarse. No abría la salida (los candados de `send.go` intactos,
  verificado a fondo por Amatista) — el semáforo mentía por omisión.
  Fix: `mode IN ('dedicated', 'auto')`, en lockstep con las otras dos.
  Auditoría pedida por Citrino ("qué otras consultas filtran por mode y
  por qué cada una está bien así"): exactamente 3 queries SQL en
  `internal/store` filtran por `mode` (las 3 en `pending.go` — ninguna
  cuarta) + un filtro en Go, no SQL, en `internal/autoreply/worker.go`
  (`p.Mode != "auto"`, correcto por diseño: ese worker es el bridge
  legacy específico de chats `auto`, opuesto en propósito al despacho
  dedicated+auto de `capipush`). Detalle completo en el reporte a
  Citrino. Test: un chat `auto` pendiente por sí solo cruza `SwampedAt` y
  frena a otro chat (`internal/capipush/capipush_test.go`,
  `TestBackpressureCountsAutoModeChats`) + test directo del contador
  (`internal/store/pending_test.go`,
  `TestCountRecentPendingNonBossIncludesAutoMode`).
  `go build ./... && go vet ./... && go test ./...` verde.
- **T22** (`ct-2026-08-05-1455`, bloqueaba la publicación — tercera
  revisión de Amatista R3, sobre T21) — dos huecos más en el instalador,
  uno en el parser, uno en la escritura.
  - **Comillas alrededor del valor:** `ParseSetLine` sacaba las comillas
    del valor (`set VAR="x"` → reusaba `x` sin comillas). cmd NO las saca
    ahí — solo cuando envuelven TODO `VAR=valor` (`set "VAR=x"`).
    Verificado ejecutando cmd de verdad (Amatista, re-verificado acá
    independiente). El caso real: alguien edita el `.bat` con
    `set PIUMY_BACKUP_KEY="miclave"` — la app cifra con `"miclave"`
    (comillas incluidas), el parser viejo reusaba `miclave` sin comillas
    (una clave DISTINTA) sin abortar, porque "se encontraba". Fix,
    dirección de Citrino: replicar cmd exacto, no interpretar — se borró
    el despojo de comillas del valor.
  - **Escritura del launcher sin verificar:** `SaveStringToFile` no
    chequeaba su resultado — un antivirus bloqueando la creación del
    `.bat` (actor real y frecuente) dejaba la instalación "exitosa" con
    la app arrancando igual, las 4 claves solo en el entorno del proceso.
    Un backup generado en esa corrida quedaba cifrado con una clave que
    muere al cerrar — la misma pérdida de R2 (T21), entrando por la
    escritura. Fix: se chequea el resultado; si falla, la app NO arranca
    y se avisa.
  - **Encontrado probando el fix de arriba:** mi primer intento avisaba
    con un `MsgBox()` incondicional — colgó el instalador para siempre en
    modo silencioso (verificado en vivo con un arnés aislado). A
    diferencia del diálogo interno de Inno por una excepción no atrapada
    (T21, sí cubierto por `/SUPPRESSMSGBOXES`), un `MsgBox()` propio
    llamado desde `[Code]` NO se auto-responde con esa flag. Mismo patrón
    que `NextButtonClick` (T8) ya usaba: `MsgBox` solo si `not
    WizardSilent`; el `Log` (incondicional) es lo único que sobrevive en
    silencioso.
  - Menores: `GenerateRandomHex` propaga error como string en vez de
    `RaiseException` (corre dentro de `PrepareToInstall` desde T21, nunca
    se había probado desde ahí); URL del tablero con `PIUMY_REST_ADDR` en
    forma `host:puerto` (no `:puerto`) ya no antepone `127.0.0.1` dos
    veces.
  - Probado en vivo: las dos formas de comillas reusan el valor idéntico
    al real; escritura bloqueada no cuelga, no arranca la app, avisa;
    tabla de T21 repasada por spot-check (limpia, UTF-16, truncado,
    `AppMutex`) — sigue verde. Instalador recompilado
    (`dist/Piumy-Setup-0.1.2.exe`, ISCC 6.7.3). Detalle completo en
    `docs/T21-DIAGRAMA-INSTALADOR-KEY-SAFETY.md` (sección T22).
- **T23** (`ct-2026-08-05-1615`, lo último que bloqueaba publicar — cuarta
  revisión de Amatista, R4) — el mismo patrón que T22 arregló en la
  escritura del launcher (`MsgBox()` propio, no cubierto por
  `/SUPPRESSMSGBOXES`, cuelga una instalación desatendida para siempre)
  quedó vivo en otros dos lugares del mismo archivo.
  - **`Exec` falla al arrancar Piumy.exe** (línea 649, 12 líneas antes del
    corte por silencioso): antivirus/SmartScreen bloqueando un ejecutable
    nuevo sin firma es igual o más probable que bloqueando la creación
    del `.bat`. Las claves ya están escritas (estado recuperable), pero
    el `MsgBox` incondicional colgaba la máquina en una instalación
    "zombie" para siempre. Fix: mismo patrón — `Log` siempre, `MsgBox`
    solo si `not WizardSilent`.
  - **Confirmación de borrar `secrets\` al desinstalar** (línea 804):
    distinto de los otros dos — es un Sí/No que decide algo irreversible,
    no un aviso informativo. En silencioso, `UninstallSilent` salta
    directo al mismo default que ya tenía el diálogo interactivo
    (`MB_DEFBUTTON2` = No) y lo registra — un retiro desatendido de
    varias máquinas nunca pierde datos por un click que nadie dio.
  - **Barrido completo de la superficie**, no solo los dos señalados:
    enumerados los 6 `MsgBox`/`RaiseException` reales del archivo — los 3
    restantes (línea 572 `RaiseException` ya verificado por T8; líneas
    582/587 inalcanzables en silencioso por construcción) confirmados
    seguros, ninguno más en el archivo (grepeado `external`/
    `SuppressibleMsgBox`/`TaskDialogMsgBox`/`Confirm(` — nada nuevo).
  - Probado en vivo: `Exec` fallando con arnés aislado (mismo patrón
    exacto, sin cuelgue, log escrito) y desinstalación silenciosa real
    con `secrets\` presente (`unins000.exe //VERYSILENT
    //SUPPRESSMSGBOXES` — exit 0, sin cuelgue, `secrets\` intacta).
    Instalador recompilado (`dist/Piumy-Setup-0.1.2.exe`, ISCC 6.7.3).
    Detalle en `docs/T21-DIAGRAMA-INSTALADOR-KEY-SAFETY.md` (sección T23).
- **T13** (`ct-2026-08-05-123147`, urgente — sin esto una instalación
  limpia nace muda) — pestaña Rules en el tablero + campo `identity` +
  las 4 reglas de fábrica, boss verbatim ("apruebo las 4 reglas").
  - **El defecto:** las 4 reglas (`rules_default`/`rules_type_group`/
    `rules_default_contact`/`rules_default_new_number`) están vacías por
    default y `EffectiveRules`'s gate duro ("sin reglas efectivas, la IA
    nunca actúa") las deja SIN NADA — verificado por API contra la
    instalación real del boss, las 4 vacías. Una instalación limpia recibe
    todo y no contesta nunca.
  - **`store.SeedFactoryRulesIfUnset()`** (`rules_seed.go`) siembra
    `identity` + las 4, con su texto de fábrica EXACTO (aprobado por el
    boss, no parafraseado), SOLO para las claves que nunca se escribieron
    — `store.KVExists(key)`, nuevo, distingue "nunca se escribió" de
    "se escribió vacío a propósito" (`KVGet` sola no puede: devuelve `""`
    para ambos casos). Llamada una vez al arrancar (`main.go`), cubre
    tanto una instalación limpia como una ya corriendo que actualiza (la
    del boss).
  - **`identity`** (nuevo campo, `SettingIdentity`): "asistente de qué"
    (boss verbatim: "una empresa de x cosa, una persona ocupada, etc."),
    arriba de las 4 reglas porque las gobierna a todas. Mismo patrón CRUD
    que las 4 — `GET/POST /api/admin/identity`.
  - **Pestaña Rules**: los 4 campos que vivían sueltos arriba de las
    pestañas del tablero (M5, ct-2026-07-22-1903) se mudaron a una
    pestaña propia junto a Chats/Grupos/Contactos/Agentes, `identity`
    arriba de todos. Guardar confirma con "✓ Guardado." sin recargar la
    página (`flashSaveResult`, mismo patrón que `config_email_save` ya
    usaba). Los selectores de nivel de "Mensajes nuevos"/"Contactos"
    (M5) se mudaron junto con su regla — siguen siendo el mismo par
    modo+reglas, no se separaron.
  - **Agregado de Citrino** (nota de regla 4 de T12): el manual del
    orquestador (`internal/mcpserver/manuals/orchestrator/SKILL.md`)
    decía en el paso 3 del setup que había que marcar al dueño a mano
    "sin esto sus propios mensajes no llegan a ningún agente y el sistema
    no avisa" — ya no es cierto para el número que parea WhatsApp (T12).
    Corregido: el que parea queda marcado solo; el paso 3 ahora es
    específicamente para UN SEGUNDO número personal del dueño, que sigue
    siendo manual a propósito (`perillas.md` también actualizado con la
    misma aclaración).
  - Probado: instalación limpia siembra las 5 con su texto de fábrica
    (verificado por API contra una instancia real, verbatim exacto);
    campo vaciado a propósito + reinicio → sigue vacío, no se resiembra;
    campo escrito por el dueño + reinicio → intacto; campo nunca tocado +
    reinicio → sigue con el de fábrica. HTML servido validado con un
    parser real (`html.parser`, sin mismatches de tags) — el navegador
    Chrome no estaba disponible en la sesión para el click-through visual.
  - `go build ./... && go vet ./... && go test ./...` verde.
- **T11** (`ct-2026-08-05-1214`, último de los que bloqueaban publicar —
  boss verbatim: "se nota que se ejecuta un bat o algo, se puede hacer eso
  silencioso?") — la pantalla negra que veía el boss al arrancar Piumy
  desde el menú inicio. Diagnóstico por eliminación (ya venía en el
  contrato, verificado): `Piumy.exe` es subsystem GUI (no abre consola
  sola), los accesos directos apuntaban a un `.vbs` que corría
  `run-piumy.bat` con ventana oculta — en Windows 11 el host de consola
  por defecto no siempre respeta ese pedido. La causa de fondo no era el
  parpadeo: había un script de shell en el camino de arranque, solo para
  setear variables de entorno — y un `.bat` de Windows no viaja a
  Linux/Mac/Raspberry.
  - **`piumy-config.json`**, archivo de configuración nuevo de ENTRADA
    (el gateway lo LEE al arrancar) — distinto a propósito de
    `agent-connect.json` (SALIDA: el gateway lo escribe para que un
    agente lo lea). `config.ApplyFileDefaults()` (`internal/config/
    filedefaults.go`) corre en `main.go` ANTES de `config.Load()`:
    rellena los `PIUMY_*` que NO estén ya seteados — la variable de
    entorno gana SIEMPRE, así que dev/`rl.bat`/tests no cambian en nada.
  - **Migración sin pérdida de claves**: si `piumy-config.json` no existe
    pero sí un `run-piumy.bat` viejo, se migra — mismo parser tolerante
    que T21/T22 escribieron para el instalador (mayús/minús, comillas sin
    despojar del valor, última coincidencia gana), portado a Go. Exige
    las 3 claves base (MCP/REST/BACKUP) o aborta sin escribir nada; `CapiKey`
    ausente se genera con `crypto/rand`.
  - **El instalador** (`piumy.iss`, `ResolveKeys`, renombrada de
    `ResolveLauncherKeys`) ahora prueba 3 fuentes en orden:
    `piumy-config.json` (ya actualizó a T11) → `run-piumy.bat` (vieja, sin
    actualizar) → generar las 4 (limpia) — las dos fuentes existentes
    comparten `FinalizeKeys` para la validación. `CurStepChanged` escribe
    `piumy-config.json` con el mismo patrón de escritura-verificada de
    T22/T23. Los accesos directos (`[Icons]`) apuntan a `Piumy.exe`
    DIRECTO — el `.vbs` deja de generarse, ya no tiene propósito. Un
    `.bat` viejo se deja intacto, sin volver a escribirlo (convenience de
    arranque manual, fuera del camino normal). Versión del paquete
    0.1.2 → 0.1.3 (cambia qué instala/cómo arranca, mismo criterio que
    T4/T2).
  - **Deuda pagada**: T6 había dejado un `ponytail:` anotando "el camino
    de upgrade es mover las 4 claves a un archivo de config propio" —
    borrado de `piumy.iss`, ya no es una nota, es lo que hay.
  - Probado en vivo contra el instalador real: instalación limpia (sin
    `.bat`/`.vbs`, acceso directo real leído con `WScript.Shell` apunta a
    `Piumy.exe`, la app responde por REST usando solo el archivo);
    migración desde un `.bat` de 4 claves (exactas, `.bat` viejo intacto);
    `piumy-config.json` ya existente gana sobre un `.bat` con otro valor
    (nunca se re-migra); BOM/truncado en `piumy-config.json` abortan
    (exit 7, cero archivos); escritura bloqueada no cuelga ni arranca la
    app; `PIUMY_DB_PATH` como variable de entorno real gana sobre el
    archivo. 10 tests de Go (`internal/config/filedefaults_test.go`).
    Detalle completo en `docs/T11-DIAGRAMA-CONFIG-FILE-NO-BAT.md`.
  - `go build ./... && go vet ./... && go test ./...` verde.
- **T24** (`ct-2026-08-05-1740`, CRÍTICO — bloqueaba publicar) — el smoke
  en la máquina real del boss (instalador 0.1.3 sobre su 0.1.1) pidió la
  clave del tablero de nuevo en una actualización real. Canceló a tiempo,
  cero archivos tocados — el aborto limpio de T21/T22/T23 funcionó — pero
  el síntoma en sí era un defecto nuevo.
  - **La causa**: `ExistingInstallDetected` confía en la carpeta de
    instalación que Inno resuelve solo — vía el AppId en el registro si lo
    reconoce, o el directorio por defecto si no. Si esa entrada de
    registro falta o quedó desactualizada (cualquier motivo, no hace falta
    saber cuál) la carpeta cae al default de siempre, y si la instalación
    real del usuario NO está ahí, todo lo que depende de esa carpeta falla
    en silencio: pide la clave de nuevo Y genera 4 claves nuevas sobre una
    instalación que ya tenía las suyas. Reproducido en aislamiento: sin
    registro + `run-piumy.bat` real en otra carpeta → exactamente ese
    desastre.
  - **El fix**: el acceso directo del menú inicio (`[Icons]`, creado
    SIEMPRE, sin `Tasks:`) sobrevive a que el registro se pierda — para
    haber arrancado la app alguna vez de verdad, tuvo que apuntar a la
    carpeta REAL. Leerlo con `WScript.Shell` en `InitializeWizard`
    (`PreviousInstallDirFromShortcut`, mismo mecanismo que T11 ya usó para
    VERIFICAR el `.lnk`, ahora para LEERLO) y, si la carpeta que Inno
    precargó no tiene instalación pero la del acceso directo sí, forzar
    `WizardForm.DirEdit.Text` a esa — de ahí en más `ExistingInstallDetected`/
    `ResolveKeys`/`PrepareToInstall` ven la carpeta correcta sin tocarlos.
  - Encontrado haciendo el fix: la constante de directorio no se puede
    expandir todavía dentro de `InitializeWizard` (error interno, probado
    en carne propia) — hay que leer `WizardForm.DirEdit.Text` directo, no
    la constante, en ese punto del ciclo de vida.
  - Probado en vivo (instalador de prueba aislado, tres escenarios):
    registro-reconoce-correctamente sigue igual; sin registro con la
    instalación real en otra carpeta, ahora reusa las 4 claves sin pedir
    nada (antes: pedía la clave y generaba 4 nuevas); instalación limpia
    real sigue generando 4 claves, sin falso positivo. Instalador
    recompilado (`dist/Piumy-Setup-0.1.3.exe`, ISCC 6.7.3) desde el
    archivo real. Detalle completo en
    `docs/T21-DIAGRAMA-INSTALADOR-KEY-SAFETY.md` (sección T24).
  - **Agregado (boss verbatim: "tiene que quedar comentado, para cuando se
    compile para mac y linux se resuelva distinto")**: comentario en
    `piumy.iss`, junto a `PreviousInstallDirFromShortcut`, deja explícito
    que el mecanismo (`WScript.Shell`, un `.lnk`) es Windows puro — lo que
    cruza a otra plataforma es el PROBLEMA (encontrar una instalación
    previa cuando el mecanismo del sistema no la reporta), no este
    mecanismo concreto.

---

## 2026-07-19 — Deploy: backup completo de WhatsApp

### Added
- Schema para backup completo de WhatsApp (`ct-2026-07-19-0102`, backup Sub 1 — SOLO
  schema, sin poblar): tabla `group_members` (miembros de grupo con nombre, para el
  scraping del boss) + columna `chats.contact_name` (nombre de agenda del teléfono,
  distinto de `chats.name`). `media.ts` verificado, ya existía.
- Backfill anti-ban de CONTACTOS (`ct-2026-07-19-0115`, backup Sub 2a — cherry-pick
  de Piumy `gateway/sync.go`, solo contactos, grupos van en el Sub 2b): al conectar
  y cada 6h, recorre todos los contactos conocidos por whatsmeow, paceado con
  `governor.DelayWindow` (mismo mecanismo anti-ban que dispatch/read) antes de cada
  contacto — nunca un volcado masivo. Guarda el nombre de AGENDA del teléfono
  (`info.FullName`/`FirstName`, nunca `PushName`) en `chats.contact_name`
  (`SetContactName`, Sub 1), sin pisar un nombre ya conocido con uno vacío.
- Scraping de MIEMBROS de grupos (`ct-2026-07-19-0138`, backup Sub 2b — extiende
  `seedGroups`): por cada grupo ya conocido al conectar, guarda sus participantes
  (número + nombre si viene) en `group_members` (`UpsertGroupMember`, Sub 1) y su
  topic en `chats.description`. Store-only, sin delay anti-ban — los participantes
  ya vienen con `GetJoinedGroups`, sin llamada extra al servidor.
- HistorySync — persistir el historial reciente que WhatsApp empuja al vincular
  (`ct-2026-07-19-0148`, backup Sub 3 — patrón oficial de whatsmeow
  `ParseWebMessage`, sin referencia de Piumy): pasivo, sin delay anti-ban.
  Escribe directo al store (texto + metadata, `Type`), nunca al canal de inbound
  — el historial no se despacha al agente. A diferencia del mensaje en vivo:
  guarda también los mensajes PROPIOS viejos (`IsFromMe` no se filtra) y NUNCA
  baja media histórica (evita flood anti-ban con miles de mensajes) — media
  histórica queda diferida a un sub futuro.

---

## 2026-07-18 (tarde) — Deploy: rediseño del formato + tool de re-cableo

### Changed
- Formato del dispatch cAPI rediseñado (`ct-2026-07-18-1851`, commit `5ae18df`): `numero` y
  `nivel` (boss/caution/danger, el nivel real del sistema) suben al header del envelope que
  CleverCoder renderiza — `app: piumy|whatsapp` / `de: <numero>, <nivel>`. El body queda solo con
  el texto (+ bloque rules.md para no-boss). Nonce recortado a 4 hex como firma `NC:<hex>`, único
  entre dispatches activos (`Gate.NonceActive`). Reemplaza `piumy:whatsapp:(numero)(is_boss)(Mensaje:…)`.

### Added
- Tool MCP `set_capi_connector` (boss-only, `ct-2026-07-18-185047`, commit `ff1a875`): re-cablea la
  antena cAPI en 1 paso desde el string pegado (`ip:puerto chat_id pin`), forzando `127.0.0.1`,
  reusando el write-path del POST admin (`store.SetCAPIConnector`).

---

## 2026-07-18 — Deploy: formato compacto + reversión de la unificación

### Added
- Formato de dispatch compacto en modo plaintext: `piumy:whatsapp:(numero)(is_boss)(Mensaje:…)`
  + nonce, reemplaza el envelope JSON `{nonce,chat_jid,level,messages}`. (ct-2026-07-18-1416)
- Candado `initiateAuthorized`: el agente solo inicia conversación a chats `is_boss` o
  `active`+rules. Cierra el vector de iniciación no autorizada. (ct-2026-07-18-1438)

### Removed
- Revertida la "identidad unificada" (F1 nombres + F2 resolución `@lid→número` + reconcile):
  el boss la canceló ("separados por número está bien"). Los chats siguen keyed por `@lid`.
  Se conservan `store.IsLIDJID` + `whatsmeow.Adapter.ResolvePN` (los usa el formato compacto).

### Fixed
- `TestControllerStopWaitsForInFlightOutboxSend`: sincronización determinística del test (no un
  fix de `Stop()`).

---

## Hitos previos (resumen)

### 2026-07-14 — Media inbound
- Bajada y persistencia de media inbound (fotos/audios/stickers) en el adapter whatsmeow +
  marcador de media en el dispatch.

### 2026-07-11 — Pivote a whatsmeow (ST-E) + hardening del gateway
- **Pivote de arquitectura:** de open-wa (cliente Node externo) a **whatsmeow** (librería Go
  embebida, `CGO_ENABLED=0`, QR en el mismo proceso). Se borró `internal/openwa`. Un solo binario.
- Gate boss no residual (exige `Ready` para `LevelBoss`), flood-guard por mensaje, metering real +
  governor anti-ban honesto, modo/estado del owner persistente.

### 2026-07-10 — Smoke real + hardening post-auditoría + dashboard
- Flujo entrante WhatsApp → pipeline → despacho y camino de vuelta (agente → `send_message` →
  WhatsApp) validados en sesión real. 5 fixes graves (anti-ban + seguridad + liveness).
  Dashboard del gateway (webview + tray Windows). Injector real cAPI (CleverInjector).

### 2026-07-09 — MVP F0→F5
- F0 esqueleto Go → F1 store/infra/routing/guard → F2 interfaz `Gateway` + corepipeline →
  F3 adaptador → F4 mcpserver (23 tools MCP, Bearer, gate por nivel, cAPI, media, metering,
  confirmation_mode, DB-admin boss-only) → F5 wiring en `main.go` + MCP sobre HTTP + config.

---
