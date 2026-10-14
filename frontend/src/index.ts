import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import {
  AppMenuItemExtension,
  defineWebApplication,
  Extension,
  useUserStore
} from '@ownclouders/web-pkg'
import { urlJoin } from '@ownclouders/web-client'
import { APPID } from './appid'
import { buildRoutes } from './routes'

export default defineWebApplication({
  setup() {
    const { $gettext } = useGettext()
    const userStore = useUserStore()

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
      routes: buildRoutes($gettext),
      extensions
    }
  }
})
