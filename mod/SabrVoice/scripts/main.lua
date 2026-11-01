-- SabrVoice - UE4SS client-side Lua mod (PoC) for SCUM 1.3.5.2
--
-- Job: notice chat lines that arrive in the chat UI and hand them to sabrvoice.dll, a tiny native module that lives INSIDE the game process
-- (no helper exe, no service, no autostart). The DLL matches the line to a pre-rendered clip of the voice pack and plays it.
-- The hand-off is a plain file (<ModDir>/data/queue.txt, one "ms<TAB>KIND<TAB>text" per line) so the DLL needs nothing from the Lua C API:
-- the mod only calls package.loadlib(<ModDir>/scripts/sabrvoice.dll, "sabrvoice_start") once; that starts the DLL's worker thread.
-- Audio is NOT played by the game engine (SCUM audio is 100% Wwise, there are no SoundWave assets).
--
-- Names used (verified from the game's paks with CUE4Parse, 1.3.5.2):
--   native class        /Script/SCUM.ChatMessageWidget      (parent of UI_ChatMessageInteractive_C)
--   BP widget           UI_ChatMessageInteractive_C         has MultiLineEditableText  _messageText
--   chat window         UI_Chat_C (native ChatWidget)       has ScrollBox              _chatScrollBox
-- NOT verified at runtime (no client available when this was written): everything below that calls the engine.
--
-- Hotkeys:  F8 = play the test line (checks the native module + audio, bypasses the game)
--           F9 = write diagnostics to UE4SS.log (is the chat widget found? how many messages?)

local VERSION = "0.2.0"
local CFG = {
  poll_ms        = 700,    -- polling interval for new chat message widgets (0 = off)
  use_notify     = true,   -- also try NotifyOnNewObject on the native class (best effort)
  test_line_id   = "npc.mama_d.greeting.0",
  queue_file     = nil,    -- default: <ModDir>/data/queue.txt
  mod_dir        = nil,    -- default: found from this script's path (override only if detection fails)
  load_native    = true,   -- false = only write the queue (for debugging)
  native_file    = "sabrvoice.dll",
  min_len        = 3,
}
pcall(function() local ok, user = pcall(require, "config"); if ok and type(user) == "table" then for k, v in pairs(user) do CFG[k] = v end end end)

local function log(msg) print(string.format("[SabrVoice] %s\n", msg)) end

-- ---- where are we?  <ModDir>/scripts/main.lua -> <ModDir>
local function detect_mod_dir()
  if CFG.mod_dir then return (CFG.mod_dir:gsub("\\", "/")) end
  local ok, src = pcall(function() return debug.getinfo(1, "S").source end)
  if ok and type(src) == "string" then
    src = src:gsub("^@", ""):gsub("\\", "/")
    local dir = src:match("^(.*)/[^/]+/[^/]+$")          -- strip scripts/main.lua
    if dir and #dir > 0 then return dir end
  end
  local ok2, d = pcall(function() return IterateGameDirectories().Game.Binaries.Win64.Mods.SabrVoice.__absolute_path end)
  if ok2 and type(d) == "string" then return (d:gsub("\\", "/")) end
  return nil
end

local MOD_DIR = detect_mod_dir()
local queue_path = CFG.queue_file or ((MOD_DIR or ".") .. "/data/queue.txt")

local function load_native()
  if not CFG.load_native then return false, "disabled by config" end
  if not MOD_DIR then return false, "mod dir unknown" end
  if type(package) ~= "table" or type(package.loadlib) ~= "function" then return false, "package.loadlib not available in this UE4SS build" end
  local dll = MOD_DIR .. "/scripts/" .. CFG.native_file
  local f, err, where = package.loadlib(dll, "sabrvoice_start")
  if not f then return false, tostring(err) .. " (" .. tostring(where) .. ") " .. dll end
  local ok, e2 = pcall(f)               -- starts the DLL's worker thread; the argument is ignored
  if not ok then return false, "sabrvoice_start failed: " .. tostring(e2) end
  return true, dll
end

local function now_ms() return math.floor(os.time() * 1000) end

local function enqueue(kind, payload)
  payload = tostring(payload or ""):gsub("[\r\n\t]+", " ")
  local f, err = io.open(queue_path, "a")
  if not f then log("cannot open queue file " .. queue_path .. ": " .. tostring(err)); return false end
  f:write(string.format("%d\t%s\t%s\n", now_ms(), kind, payload))
  f:close()
  return true
end

-- text of a chat message widget (UI_ChatMessageInteractive_C)
local function widget_text(w)
  local ok, res = pcall(function()
    local t = w._messageText:GetText()
    return t:ToString()
  end)
  if ok and type(res) == "string" then return res end
  return nil
end

local seen = {}          -- widget address -> true
local seeded = false

local function consider(w, from)
  local ok, addr = pcall(function() return w:GetAddress() end)
  if not ok then return end
  if seen[addr] then return end
  seen[addr] = true
  local text = widget_text(w)
  if text and #text >= CFG.min_len then
    if seeded then enqueue("CHAT", text) end   -- never replay history that existed before the mod started
  end
end

local chat_ref = nil
local function find_chat()
  if chat_ref ~= nil then
    local ok, valid = pcall(function() return chat_ref:IsValid() end)
    if ok and valid then return chat_ref end
  end
  local ok, w = pcall(FindFirstOf, "UI_Chat_C")
  if ok and w and w:IsValid() then chat_ref = w; return w end
  chat_ref = nil
  return nil
end

local function scan_chat()
  local chat = find_chat()
  if not chat then return 0 end
  local ok, n = pcall(function() return chat._chatScrollBox:GetChildrenCount() end)
  if not ok or type(n) ~= "number" then return 0 end
  for i = 0, n - 1 do
    local ok2, child = pcall(function() return chat._chatScrollBox:GetChildAt(i) end)
    if ok2 and child and child:IsValid() then consider(child, "scan") end
  end
  return n
end

-- (1) polling: robust, only needs the widget names above
if CFG.poll_ms and CFG.poll_ms > 0 then
  ExecuteWithDelay(8000, function()
    ExecuteInGameThread(function() scan_chat(); seeded = true; log("history seeded; watching chat") end)
    LoopAsync(CFG.poll_ms, function()
      ExecuteInGameThread(function() pcall(scan_chat) end)
      return false  -- keep looping
    end)
  end)
else
  seeded = true
end

-- (2) best effort: react immediately when the engine creates a new chat message widget
if CFG.use_notify then
  local ok, err = pcall(function()
    NotifyOnNewObject("/Script/SCUM.ChatMessageWidget", function(w)
      ExecuteWithDelay(120, function()            -- text is assigned just after construction
        ExecuteInGameThread(function() if w:IsValid() then consider(w, "notify") end end)
      end)
    end)
  end)
  log(ok and "NotifyOnNewObject registered" or ("NotifyOnNewObject unavailable: " .. tostring(err)))
end

-- hotkeys
pcall(function()
  RegisterKeyBind(Key.F8, function()
    log("F8: test line -> " .. CFG.test_line_id .. "  (log: " .. tostring(MOD_DIR) .. "/data/sabrvoice.log)")
    enqueue("PING", "F8")
    enqueue("PLAY", CFG.test_line_id)
  end)
  RegisterKeyBind(Key.F9, function()
    ExecuteInGameThread(function()
      local chat = find_chat()
      log("F9: chat window found = " .. tostring(chat ~= nil))
      if chat then
        local ok, n = pcall(function() return chat._chatScrollBox:GetChildrenCount() end)
        log("F9: scrollbox children = " .. tostring(ok and n or ("error: " .. tostring(n))))
        if ok and n and n > 0 then
          local c = chat._chatScrollBox:GetChildAt(n - 1)
          log("F9: last message text = " .. tostring(c and widget_text(c)))
        end
      end
      log("F9: queue file = " .. queue_path)
    end)
  end)
end)

local native_ok, native_msg = load_native()
log("loaded v" .. VERSION .. "; mod dir = " .. tostring(MOD_DIR) .. "; queue = " .. queue_path)
log("native module: " .. (native_ok and "started (" .. tostring(native_msg) .. ")" or ("NOT loaded: " .. tostring(native_msg))))
enqueue("PING", "loaded native=" .. tostring(native_ok))
