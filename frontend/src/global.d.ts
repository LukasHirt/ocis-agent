// The oCIS design system registers the oc-* components globally at runtime;
// the extension does not bundle their types, so declare the ones it uses.
export {}

declare module 'vue' {
  interface GlobalComponents {
    OcButton: any
    OcIcon: any
    OcSpinner: any
    OcTextInput: any
    OcSelect: any
    OcCheckbox: any
    OcSwitch: any
    OcTableSimple: any
    OcTag: any
    OcModal: any
    OcNotificationMessage: any
  }
}
