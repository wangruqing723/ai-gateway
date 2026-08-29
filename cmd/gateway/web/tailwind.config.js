/** Tailwind 构建配置。此前这份内容内联在 index.html 的 <script> 里喂给 Play CDN，
 *  改成构建期生成后移到这里，色板、字体族、圆角与阴影必须与原值逐字一致——S3-2 是纯替换，
 *  任何色差或间距变化都算回归。
 *
 *  content 只扫 index.html：页面是单文件，Alpine 表达式、内联 <script> 里的类名字符串
 *  都在这个文件的原始文本里，CLI 的正则提取器能覆盖。
 *  产物由 `make web-css` 生成到 web/vendor/tailwind.css 并随仓库提交，
 *  运行镜像因此不需要 node 工具链。
 */
module.exports = {
    darkMode: 'class',
    content: ['./index.html'],
    theme: {
        extend: {
            colors: {
                ink: '#090a0f',
                panel: '#12131c',
                panel2: '#191b25',
                panel3: '#22242f',
                line: 'rgba(255,255,255,.09)',
                muted: '#9da6b5',
                text: '#e7eaf0',
                cyan: '#47d6ff',
                cyan2: '#00d2ff',
                violet: '#8f92ff',
                good: '#34d399',
                warn: '#fbbf24',
                danger: '#fb7185'
            },
            fontFamily: {
                sans: ['Inter', 'ui-sans-serif', 'system-ui'],
                mono: ['JetBrains Mono', 'ui-monospace', 'SFMono-Regular']
            },
            borderRadius: {
                lg: '8px',
                xl: '8px',
                '2xl': '8px'
            },
            boxShadow: {
                glow: '0 0 0 1px rgba(71,214,255,.12), 0 16px 48px rgba(0,0,0,.28)'
            }
        }
    }
};
