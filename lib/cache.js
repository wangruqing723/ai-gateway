/**
 * SQLite 图片缓存模块
 * 持久化存储图片识别结果，支持自动清理
 */

import initSqlJs from 'sql.js';
import fs from 'fs';
import path from 'path';
import { createHash } from 'crypto';

const DB_PATH = path.join(process.cwd(), 'data', 'image-cache.db');

let db = null;
let dbLastModified = 0;  // 数据库文件最后修改时间
let SQL = null;          // sql.js 实例
let hasUnsavedChanges = false;  // 是否有未保存的修改

// 数据库操作队列，防止并发访问
let dbQueue = Promise.resolve();

/**
 * 将数据库操作加入队列，串行执行
 * @param {Function} fn - 数据库操作函数
 * @returns {Promise}
 */
function withDB(fn) {
  return new Promise((resolve, reject) => {
    dbQueue = dbQueue.then(async () => {
      try {
        const result = await fn();
        resolve(result);
      } catch (e) {
        reject(e);
      }
    });
  });
}

// 初始化数据库
export async function initCache() {
  // 确保 data 目录存在
  const dataDir = path.dirname(DB_PATH);
  if (!fs.existsSync(dataDir)) {
    fs.mkdirSync(dataDir, { recursive: true });
  }

  if (!SQL) {
    SQL = await initSqlJs();
  }

  // 如果数据库文件存在则加载，否则创建新数据库
  if (fs.existsSync(DB_PATH)) {
    const buffer = fs.readFileSync(DB_PATH);
    db = new SQL.Database(buffer);
    dbLastModified = fs.statSync(DB_PATH).mtimeMs;
  } else {
    db = new SQL.Database();
    dbLastModified = 0;
  }

  // 创建表
  db.run(`
    CREATE TABLE IF NOT EXISTS image_cache (
      hash TEXT PRIMARY KEY,
      description TEXT NOT NULL,
      created_at INTEGER DEFAULT (strftime('%s', 'now')),
      last_used_at INTEGER DEFAULT (strftime('%s', 'now'))
    )
  `);

  // 创建索引
  db.run('CREATE INDEX IF NOT EXISTS idx_created_at ON image_cache(created_at)');

  saveDB();
  return db;
}

// 保存数据库到文件
function saveDB() {
  if (!db) return;
  const data = db.export();
  const buffer = Buffer.from(data);
  fs.writeFileSync(DB_PATH, buffer);
  dbLastModified = fs.statSync(DB_PATH).mtimeMs;
  hasUnsavedChanges = false;
}

// 检查数据库文件是否被外部修改，如果是则重新加载
function checkAndReloadDB() {
  if (!SQL || !fs.existsSync(DB_PATH)) return;

  try {
    const currentModified = fs.statSync(DB_PATH).mtimeMs;
    if (currentModified > dbLastModified) {
      // 数据库文件被外部修改，重新加载
      const buffer = fs.readFileSync(DB_PATH);
      if (db) db.close();
      db = new SQL.Database(buffer);
      dbLastModified = currentModified;
      hasUnsavedChanges = false;
    }
  } catch (e) {
    // 忽略检查错误
  }
}

/**
 * 生成图片哈希（使用 SHA-256，更安全）
 * @param {object} imageBlock - Anthropic 格式的 image 块
 * @returns {string} 哈希值（前 32 字符）
 */
export function getImageHash(imageBlock) {
  const src = imageBlock.source || {};
  const raw = src.type === 'base64'
    ? `${src.media_type}_${src.data || ''}`  // 使用完整数据
    : (src.url || src.image_url?.url || '');

  return createHash('sha256')
    .update(raw)
    .digest('hex')
    .slice(0, 32);  // 取前 32 字符
}

/**
 * 查询缓存
 * @param {string} hash - 图片哈希
 * @returns {Promise<string|null>} 缓存的描述或 null
 */
export function getCachedDescription(hash) {
  return withDB(() => {
    if (!db) return null;

    // 检查数据库文件是否被外部修改
    checkAndReloadDB();

    const stmt = db.prepare('SELECT description FROM image_cache WHERE hash = ?');
    stmt.bind([hash]);

    if (stmt.step()) {
      const row = stmt.getAsObject();
      stmt.free();

      // 更新最后使用时间
      db.run('UPDATE image_cache SET last_used_at = strftime(\'%s\', \'now\') WHERE hash = ?', [hash]);
      hasUnsavedChanges = true;
      saveDB();

      return row.description;
    }

    stmt.free();
    return null;
  });
}

/**
 * 存入缓存
 * @param {string} hash - 图片哈希
 * @param {string} description - 图片描述
 * @returns {Promise<void>}
 */
export function setCachedDescription(hash, description) {
  return withDB(() => {
    if (!db) return;

    db.run(
      'INSERT OR REPLACE INTO image_cache (hash, description, created_at, last_used_at) VALUES (?, ?, strftime(\'%s\', \'now\'), strftime(\'%s\', \'now\'))',
      [hash, description]
    );
    hasUnsavedChanges = true;
    saveDB();
  });
}

/**
 * 清理过期记录
 * @param {number} maxAgeDays - 最大保留天数
 * @param {number} maxRecords - 最大记录数
 * @returns {Promise<{deleted: number, remaining: number}>}
 */
export function cleanupCache(maxAgeDays = 7, maxRecords = 1000) {
  return withDB(() => {
    if (!db) return { deleted: 0, remaining: 0 };

    const maxAgeSeconds = maxAgeDays * 24 * 60 * 60;

    // 删除过期记录
    db.run('DELETE FROM image_cache WHERE created_at < strftime(\'%s\', \'now\') - ?', [maxAgeSeconds]);
    const deletedByAge = db.getRowsModified();

    // 保留最新的 N 条记录
    db.run(`
      DELETE FROM image_cache WHERE hash NOT IN (
        SELECT hash FROM image_cache ORDER BY created_at DESC LIMIT ?
      )
    `, [maxRecords]);
    const deletedByCount = db.getRowsModified();

    const totalDeleted = deletedByAge + deletedByCount;

    // 如果有删除记录，执行 VACUUM 压缩数据库
    if (totalDeleted > 0) {
      db.run('VACUUM');
    }

    saveDB();

    // 查询剩余记录数
    const result = db.exec('SELECT COUNT(*) as count FROM image_cache');
    const remaining = result[0]?.values[0]?.[0] || 0;

    return {
      deleted: totalDeleted,
      remaining
    };
  });
}

/**
 * 获取缓存统计
 * @returns {Promise<{total: number, contentSize: number, dbSize: number}>}
 */
export function getCacheStats() {
  return withDB(() => {
    if (!db) return { total: 0, contentSize: 0, dbSize: 0 };

    // 检查数据库文件是否被外部修改
    checkAndReloadDB();

    // 获取记录数
    const countResult = db.exec('SELECT COUNT(*) as count FROM image_cache');
    const total = countResult[0]?.values[0]?.[0] || 0;

    // 获取缓存内容实际大小（所有 description 字段的总字节数）
    let contentSize = 0;
    if (total > 0) {
      const sizeResult = db.exec('SELECT COALESCE(SUM(LENGTH(description)), 0) as total_size FROM image_cache');
      contentSize = sizeResult[0]?.values[0]?.[0] || 0;
    }

    // 数据库文件总大小
    const dbSize = fs.existsSync(DB_PATH) ? fs.statSync(DB_PATH).size : 0;

    return { total, contentSize, dbSize };
  });
}

/**
 * 清空所有缓存
 * @returns {Promise<number>} 删除的记录数
 */
export function clearAllCache() {
  return withDB(() => {
    if (!db) return 0;

    db.run('DELETE FROM image_cache');
    const deleted = db.getRowsModified();

    // 执行 VACUUM 压缩数据库文件，释放空间
    db.run('VACUUM');
    saveDB();

    return deleted;
  });
}

// 关闭数据库
export function closeCache() {
  if (db) {
    // 只有在有未保存的修改时才写入文件，避免覆盖外部修改
    if (hasUnsavedChanges) {
      saveDB();
    }
    db.close();
    db = null;
  }
}
