<template>
  <main class="mx-auto max-w-lg p-8 text-center">
    <h1 class="mb-4 text-2xl font-semibold">IP 暂未授权</h1>
    <p class="mb-6 text-gray-500">当前访问 IP：{{ ip || '检测中...' }}</p>
    <button class="rounded bg-blue-600 px-4 py-2 text-white disabled:opacity-50" :disabled="loading || !ip" @click="submit">
      {{ loading ? '提交中...' : '提交当前 IP' }}
    </button>
    <p v-if="message" class="mt-4 text-green-600">{{ message }}</p>
    <p v-if="error" class="mt-4 text-red-600">{{ error }}</p>
  </main>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
const ip = ref(''); const loading = ref(false); const message = ref(''); const error = ref('')
onMounted(async () => { try { const r = await fetch('/ip-access-request'); const d = await r.json(); ip.value = d.ip || '' } catch { error.value = '无法检测当前 IP' } })
async function submit() { loading.value = true; error.value = ''; try { const r = await fetch('/api/v1/ip-access-requests', { method: 'POST' }); const d = await r.json(); if (!r.ok) throw new Error(d.message || '提交失败'); message.value = `IP ${d.ip} 已加入白名单，请刷新页面` } catch (e) { error.value = e instanceof Error ? e.message : '提交失败' } finally { loading.value = false } }
</script>
