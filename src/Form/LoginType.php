<?php

namespace App\Form;

use Symfony\Component\Form\AbstractType;
use Symfony\Component\Form\Extension\Core\Type\{PasswordType, TextType};
use Symfony\Component\Form\FormBuilderInterface;

class LoginType extends AbstractType
{
    public function buildForm(FormBuilderInterface $builder, array $options)
    {
        $builder
            ->add('_username', TextType::class, ['mapped' => false])
            ->add('_password', PasswordType::class, ['mapped' => false])
        ;
    }

    public function getBlockPrefix()
    {
        // Empty prefix for default UsernamePasswordFrormAuthenticationListener
        return '';
    }


}