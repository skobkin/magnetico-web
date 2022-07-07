<?php

namespace App\Form;

use App\Form\Data\PasswordResetRequestData;
use Symfony\Component\Form\{AbstractType, Extension\Core\Type\EmailType, FormBuilderInterface};
use MeteoConcept\HCaptchaBundle\Form\HCaptchaType;
use Symfony\Component\OptionsResolver\OptionsResolver;

class PasswordResetRequestType extends AbstractType
{
    public function buildForm(FormBuilderInterface $builder, array $options)
    {
        $builder
            ->add('email', EmailType::class, ['required' => true])
            ->add('captcha', HCaptchaType::class)
        ;
    }

    public function configureOptions(OptionsResolver $resolver)
    {
        $resolver->setDefaults([
            'data_class' => PasswordResetRequestData::class,
        ]);
    }
}
