import { minimatch } from 'minimatch';

/**
 * 根据请求中的模型名，匹配路由规则
 * @param {string} model  - 客户端发来的原始模型名
 * @param {object} config - 完整配置
 * @returns {{ route, provider, visionProvider? }} 匹配结果
 */
export function matchRoute(model, config) {
  for (const route of config.routes) {
    if (minimatch(model, route.match, { nocase: true })) {
      const provider = config.providers[route.provider];
      const result = {
        route,
        provider: { name: route.provider, ...provider },
        targetModel: route.model || model,
      };
      if (route.vision) {
        result.visionProvider = {
          name: route.vision.provider,
          model: route.vision.model,
          ...config.providers[route.vision.provider],
        };
      }
      return result;
    }
  }
  return null;
}

/**
 * 从请求或配置中解析最终使用的 API Key
 * 优先级：provider.apiKey > 请求头
 */
export function resolveApiKey(provider, requestHeaders) {
  if (provider.apiKey) return provider.apiKey;
  return requestHeaders['x-api-key']
    || (requestHeaders['authorization'] || '').replace(/^Bearer\s+/i, '')
    || '';
}
