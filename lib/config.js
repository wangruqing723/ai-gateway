import { readFileSync, existsSync, writeFileSync, mkdirSync } from 'fs';
import { join, dirname } from 'path';
import { homedir } from 'os';
import { fileURLToPath } from 'url';
import yaml from 'js-yaml';

const __dirname = dirname(fileURLToPath(import.meta.url));
const ROOT = join(__dirname, '..');

// 配置文件查找顺序（低 → 高优先级）
const CONFIG_PATHS = [
  join(homedir(), '.config', 'ai-gateway', 'config.yaml'),
  join(ROOT, 'config.yaml'),
];

export function findConfigPath() {
  // 优先用项目目录下的 config.yaml，其次用全局配置
  for (let i = CONFIG_PATHS.length - 1; i >= 0; i--) {
    if (existsSync(CONFIG_PATHS[i])) return CONFIG_PATHS[i];
  }
  return null;
}

export function initConfig() {
  const target = CONFIG_PATHS[CONFIG_PATHS.length - 1]; // 项目目录
  if (existsSync(target)) {
    console.log(`配置文件已存在: ${target}`);
    return;
  }
  const example = join(ROOT, 'config.example.yaml');
  const content = existsSync(example)
    ? readFileSync(example, 'utf-8')
    : defaultConfigContent();
  writeFileSync(target, content, 'utf-8');
  console.log(`✓ 已生成配置文件: ${target}\n  请编辑后启动 gateway。`);
}

export function loadConfig() {
  const configPath = findConfigPath();
  if (!configPath) {
    console.error(
      '[gateway] 未找到配置文件。\n' +
      `  运行 'node gateway.js --init' 生成默认配置，\n` +
      `  或将 config.example.yaml 复制为 config.yaml 并填写。`
    );
    process.exit(1);
  }

  let raw;
  try {
    raw = yaml.load(readFileSync(configPath, 'utf-8'));
  } catch (e) {
    console.error(`[gateway] 配置文件解析失败 (${configPath}): ${e.message}`);
    process.exit(1);
  }

  return validate(raw, configPath);
}

function validate(raw, configPath) {
  if (!raw.providers || typeof raw.providers !== 'object') {
    die(configPath, 'providers 字段缺失或格式错误');
  }
  if (!Array.isArray(raw.routes) || raw.routes.length === 0) {
    die(configPath, 'routes 字段缺失或为空数组');
  }

  // 校验每个 provider
  for (const [name, p] of Object.entries(raw.providers)) {
    if (!p.baseUrl) die(configPath, `providers.${name}.baseUrl 缺失`);
    if (!['anthropic', 'openai'].includes(p.format)) {
      die(configPath, `providers.${name}.format 须为 anthropic 或 openai`);
    }
  }

  // 校验每条路由
  for (const route of raw.routes) {
    if (!route.match) die(configPath, 'route 缺少 match 字段');
    if (!route.provider) die(configPath, `route "${route.match}" 缺少 provider 字段`);
    if (!raw.providers[route.provider]) {
      die(configPath, `route "${route.match}" 引用了未定义的 provider: ${route.provider}`);
    }
    if (route.vision) {
      if (!raw.providers[route.vision.provider]) {
        die(configPath, `route "${route.match}".vision 引用了未定义的 provider: ${route.vision.provider}`);
      }
    }
  }

  return {
    port:      raw.port      || 7789,
    providers: raw.providers,
    routes:    raw.routes,
    _path:     configPath,
  };
}

function die(path, msg) {
  console.error(`[gateway] 配置错误 (${path}): ${msg}`);
  process.exit(1);
}

function defaultConfigContent() {
  return `port: 7789\n\nproviders:\n  my_provider:\n    baseUrl: "https://api.example.com"\n    apiKey: "sk-xxx"\n    format: anthropic\n\nroutes:\n  - match: "*"\n    provider: my_provider\n    model: "target-model-name"\n`;
}
