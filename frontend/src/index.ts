import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import {
  AppMenuItemExtension,
  defineWebApplication,
  Extension,
  useAuthStore,
  useMessages,
  useModals,
  useUserStore
} from '@ownclouders/web-pkg'
import { urlJoin } from '@ownclouders/web-client'
import { APPID } from './appid'
import { buildRoutes } from './routes'
import { createLayout } from './layout'
import { createPlatform } from './platform'

export default defineWebApplication({
  setup({ applicationConfig }) {
    const { $gettext } = useGettext()
    const userStore = useUserStore()
    const authStore = useAuthStore()
    const messages = useMessages()
    const modals = useModals()

    // The only place that touches the oCIS Web runtime: everything else
    // gets it through the platform the layout provides.
    const platform = createPlatform({
      accessToken: () => authStore.accessToken,
      backendUrl: applicationConfig?.backendUrl,
      fetch: window.fetch.bind(window),
      showMessage: (m) => messages.showMessage(m),
      showErrorMessage: (m) => messages.showErrorMessage(m),
      dispatchModal: (m) => modals.dispatchModal(m)
    })

    const appInfo = {
      name: $gettext('Agent'),
      id: APPID,
      icon: 'robot',
      color: '#2f6fed'
    }

    const menuItemExtension: AppMenuItemExtension = {
      id: `app.${appInfo.id}.menuItem`,
      type: 'appMenuItem',
      label: () => appInfo.name,
      color: appInfo.color,
      icon: appInfo.icon,
      priority: 40,
      path: urlJoin(appInfo.id)
    }

    const extensions = computed(() => {
      const result: Extension[] = []
      if (userStore.user) {
        result.push(menuItemExtension)
      }
      return result
    })

    return {
      appInfo,
      routes: buildRoutes($gettext, createLayout(platform)),
      extensions
    }
  }
})
